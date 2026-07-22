package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// WantedSource is the slice of WantedSync that Discovery needs: the most
// recently synced wanted-missing snapshot, keyed by Lidarr album ID. A
// narrow interface rather than *WantedSync itself so tests can fake it
// without constructing a real WantedSync.
type WantedSource interface {
	Wanted() map[int64]core.WantedRelease
}

// DiscoveryStore is the slice of the store Discovery needs. It embeds
// BackoffStore since a search cycle with zero surviving candidates goes
// through the same failOrBackoff path as every other module.
type DiscoveryStore interface {
	BackoffStore
	// RunnableJobsInState is used with StateWanted to pick the single job a
	// tick searches for (see RunnableJobsInState's doc comment for ordering).
	RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
	// InsertCandidates caches a job's surviving ranked candidates and resets
	// its search cycle (retries=0, not_before=NULL) in the same transaction.
	InsertCandidates(ctx context.Context, jobID int64, cands []store.NewCandidate, now time.Time) error
	AdvanceJobStateFrom(ctx context.Context, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error)
	// ReliabilityFor batch-looks-up known peer reliability history for a set
	// of usernames against one artist, for use in Ranker.Rank.
	ReliabilityFor(ctx context.Context, artistID int64, usernames []string) (map[string]core.PeerReliability, error)
	// SetJobTrackBand caches the album's valid track-count band on the job,
	// read later by Importing's coverage gate.
	SetJobTrackBand(ctx context.Context, jobID int64, minTracks, maxTracks int) error
	// RecordSearchPass appends one row recording a completed Discovery search
	// cycle, for the Overview charts (see Tick's best-effort recording).
	RecordSearchPass(ctx context.Context, p core.SearchPass) error
}

// DiscoveryParams configures a Discovery.
type DiscoveryParams struct {
	Store        DiscoveryStore
	Peers        PeerSearcher
	Music        MusicSource
	Ranker       Ranker
	WantedSource WantedSource

	SearchTimeout time.Duration
	MaxCandidates int
	MaxRetries    int
	BackoffBase   time.Duration
	BackoffCap    time.Duration
	Interval      time.Duration

	Logger *slog.Logger
}

// Discovery searches for one WANTED album per tick, caches the ranked,
// filtered candidates, and advances the job to SELECTING. It ports the front
// half of the legacy engine's startJob (up to but not including candidate
// selection/enqueue, which Selecting now owns).
type Discovery struct {
	p DiscoveryParams
}

// NewDiscovery constructs a Discovery.
func NewDiscovery(p DiscoveryParams) *Discovery {
	if p.Logger != nil {
		p.Logger = p.Logger.With("module", "discovery")
	}
	return &Discovery{p: p}
}

// Name identifies this module in logs and Health().
func (d *Discovery) Name() string { return "discovery" }

// Interval is how often this module ticks.
func (d *Discovery) Interval() time.Duration { return d.p.Interval }

func (d *Discovery) log() *slog.Logger {
	if d.p.Logger != nil {
		return d.p.Logger
	}
	return slog.Default()
}

// recordEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it
// is logged at warn level and swallowed rather than propagated (same pattern
// as engine.Discoverer.recordEvent).
func (d *Discovery) recordEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) {
	if err := d.p.Store.AddJobEvent(ctx, jobID, event, detail, now); err != nil {
		d.log().Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
	}
}

// Tick processes exactly one runnable WANTED job (the highest-release-date
// one) per call. One search per tick is deliberate pacing: at a 30s tick
// interval this bounds how fast the pipeline burns through Soulseek search
// traffic and candidate budget, independent of how many albums are wanted at
// once - a large backlog drains steadily rather than hammering peers all at
// once the moment it appears.
func (d *Discovery) Tick(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.RunnableJobsInState(ctx, core.StateWanted, now, 1)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	completed, matched, err := d.searchJob(ctx, jobs[0], now)
	if err != nil {
		return err
	}
	if completed {
		d.recordSearchPass(ctx, matched, now)
	}
	return nil
}

// recordSearchPass best-effort appends one row recording a completed
// Discovery search cycle, for the Overview charts (issue #88). A write
// failure must never block the pipeline, so it is logged at warn level and
// swallowed rather than propagated (same pattern as recordEvent).
func (d *Discovery) recordSearchPass(ctx context.Context, matched bool, now time.Time) {
	pass := core.SearchPass{StartedAt: now, FinishedAt: time.Now(), Searched: 1}
	if matched {
		pass.Matched = 1
	}
	if err := d.p.Store.RecordSearchPass(ctx, pass); err != nil {
		d.log().Warn("record search pass failed", "err", err)
	}
}

// searchJob searches for one album, ranks and filters the results, and
// either caches the survivors and advances the job to SELECTING, or backs it
// off (still WANTED) when nothing survives. completed reports whether the
// search cycle ran to one of its two normal conclusions (backed off or
// matched) rather than aborting early (album missing from the wanted
// snapshot, a search error, or an AlbumReleases error) - Tick only records a
// search pass when completed is true. matched is true only once
// InsertCandidates has cached surviving candidates.
func (d *Discovery) searchJob(ctx context.Context, job core.AlbumJob, now time.Time) (completed, matched bool, err error) {
	album, ok := d.p.WantedSource.Wanted()[job.LidarrAlbumID]
	if !ok {
		// The album is missing from the most recent wanted snapshot. Unlike the
		// legacy engine, Discovery never cancels a job itself - WantedSync owns
		// CANCELLED and will catch this on its own next sync. Just skip this
		// tick and let the job be picked up again once metadata resolves (or
		// cancelled).
		d.log().Info("album missing from wanted snapshot, skipping",
			"album_job", job.ID, "lidarr_album", job.LidarrAlbumID)
		return false, false, nil
	}

	query := album.ArtistName + " " + album.Title
	results, err := d.p.Peers.Search(ctx, query, d.p.SearchTimeout)
	if err != nil {
		return false, false, err
	}
	if len(results) == 0 {
		// The primary query returned no raw results at all (not just no
		// candidates after ranking/filtering - that's the results' fault, not
		// the query's). Try once with a looser, normalized query: peers'
		// shared folder names rarely carry suffixes like "(Deluxe Edition)" or
		// characters like "&" verbatim, so stripping them can turn a zero-hit
		// search into a match. Skipped entirely when normalizing is a no-op
		// (to avoid doubling search traffic for nothing) or reduces the query
		// to nothing (searching for "" is meaningless).
		if fallback := normalizeQuery(query); fallback != "" && fallback != query {
			fallbackDetail := fmt.Sprintf("primary search empty, trying normalized query %q", fallback)
			d.log().Info(fallbackDetail, "album_job", job.ID, "query", fallback)
			d.recordEvent(ctx, job.ID, core.EventSearchFallback, fallbackDetail, now)
			results, err = d.p.Peers.Search(ctx, fallback, d.p.SearchTimeout)
			if err != nil {
				return false, false, err
			}
			query = fallback
		}
	}

	rel, err := d.p.Store.ReliabilityFor(ctx, job.ArtistID, uniqueUsernames(results))
	if err != nil {
		return false, false, err
	}
	ranked := d.p.Ranker.Rank(results, rel, now)
	searchDetail := fmt.Sprintf("searched album, query=%q results=%d candidates=%d", query, len(results), len(ranked))
	d.log().Info(searchDetail, "album_job", job.ID, "query", query,
		"results", len(results), "candidates", len(ranked))
	d.recordEvent(ctx, job.ID, core.EventSearch, searchDetail, now)

	// Fetch the album's releases once per searchJob call to compute the valid
	// track-count band [min, max] across all editions — a candidate matching
	// any real edition's track count is viable, since manual import runs with
	// release switching enabled and Lidarr picks the matching edition itself.
	// A (0,0) band means Lidarr has no usable release data right now, so the
	// filter is skipped entirely rather than risk rejecting a legitimate
	// candidate on bad data. An error here is not counted against the job's
	// retry budget: it aborts this search pass early - log and return nil so
	// the job stays WANTED, untouched, and is retried on a later tick.
	releases, err := d.p.Music.AlbumReleases(ctx, job.LidarrAlbumID)
	if err != nil {
		d.log().Error("album releases failed", "album_job", job.ID, "err", err)
		return false, false, nil
	}
	minTracks, maxTracks := trackBand(releases)
	// Persisted (not just used inline) because Importing's coverage gate needs
	// MinTrackCount long after this search: a candidate covering the smallest
	// valid edition must not be rejected against the canonical (larger) count.
	if err := d.p.Store.SetJobTrackBand(ctx, job.ID, minTracks, maxTracks); err != nil {
		return false, false, err
	}

	// Candidates are cached per search cycle - the previously-tried-username
	// filter the legacy engine applied at enqueue time is deliberately absent
	// here: a fresh cache is wiped on every retry cycle (see InsertCandidates
	// resetting retries, and ResetJobToWanted deleting prior candidates), so
	// there is no cross-cycle "already tried" state left to consult.
	var survivors []store.NewCandidate
	var tooManyTracks, tooFewTracks int
	for _, cand := range ranked {
		if len(survivors) >= d.p.MaxCandidates {
			break
		}
		if maxTracks > 0 && len(cand.Files) > maxTracks {
			// More files than the largest known edition — almost certainly not
			// a single release (e.g. a whole discography in one flat folder).
			detail := fmt.Sprintf("candidate %s has more files than any known release (%d files, max %d), skipping", cand.Username, len(cand.Files), maxTracks)
			d.log().Debug(detail, "album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "max", maxTracks)
			tooManyTracks++
			continue
		}
		if minTracks > 0 && len(cand.Files) < minTracks {
			// Can't cover even the smallest edition — guaranteed to fail the
			// IMPORTING coverage gate after burning a full download cycle.
			detail := fmt.Sprintf("candidate %s has fewer files than the smallest release (%d files, min %d), skipping", cand.Username, len(cand.Files), minTracks)
			d.log().Debug(detail, "album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "min", minTracks)
			tooFewTracks++
			continue
		}
		survivors = append(survivors, newCandidateFrom(cand))
	}

	if rejected := tooManyTracks + tooFewTracks; rejected > 0 {
		detail := fmt.Sprintf("rejected %d candidates: %d above maximum track count, %d below minimum track count", rejected, tooManyTracks, tooFewTracks)
		d.log().Info(detail, "album_job", job.ID, "rejected", rejected,
			"above_max_tracks", tooManyTracks, "below_min_tracks", tooFewTracks)
		d.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
	}

	if len(survivors) == 0 {
		// The EventSearch event recorded above already carries this cycle's
		// empty outcome (results/candidates counts); nothing further to record.
		d.log().Info("no viable candidates, backing off", "album_job", job.ID)
		return true, false, failOrBackoff(ctx, d.p.Store, d.log(), job, d.p.MaxRetries, d.p.BackoffBase, d.p.BackoffCap, false, now)
	}

	if err := d.p.Store.InsertCandidates(ctx, job.ID, survivors, now); err != nil {
		return false, false, err
	}
	advanced, err := d.p.Store.AdvanceJobStateFrom(ctx, job.ID, core.StateWanted, core.StateSelecting, now)
	if err != nil {
		return false, false, err
	}
	if !advanced {
		// The job left WANTED underneath us (e.g. WantedSync cancelled it
		// between RunnableJobsInState and here). The candidates just inserted
		// are inert rows on a job nothing will ever pick up again - acceptable,
		// same as a cancelled job's stale candidate_attempts under the legacy
		// engine.
		d.log().Info("job left WANTED before candidates could be applied, leaving candidates in place",
			"album_job", job.ID)
	}
	return true, true, nil
}

// newCandidateFrom converts a ranked core.RankedCandidate into the store's
// persisted candidate shape.
func newCandidateFrom(cand core.RankedCandidate) store.NewCandidate {
	files := make([]core.CandidateFile, len(cand.Files))
	for i, f := range cand.Files {
		files[i] = core.CandidateFile{Filename: f.Filename, Size: f.Size}
	}
	return store.NewCandidate{Username: cand.Username, Score: cand.Score, Files: files}
}

// trackBand computes the valid track-count band across an album's releases:
// the smallest and largest positive track count. Releases with no track count
// (0) are ignored; (0, 0) means no usable release data at all.
func trackBand(releases []core.AlbumRelease) (minTracks, maxTracks int) {
	for _, r := range releases {
		if r.TrackCount <= 0 {
			continue
		}
		if minTracks == 0 || r.TrackCount < minTracks {
			minTracks = r.TrackCount
		}
		if r.TrackCount > maxTracks {
			maxTracks = r.TrackCount
		}
	}
	return minTracks, maxTracks
}

// uniqueUsernames returns the distinct usernames present in results, in
// first-seen order, used to batch-fetch reliability history for exactly the
// peers a search actually returned rather than one query per candidate.
// Ported from internal/engine/discovery.go.
func uniqueUsernames(results []core.SearchResult) []string {
	seen := make(map[string]bool, len(results))
	out := make([]string, 0, len(results))
	for _, r := range results {
		if !seen[r.Username] {
			seen[r.Username] = true
			out = append(out, r.Username)
		}
	}
	return out
}
