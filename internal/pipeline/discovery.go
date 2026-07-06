package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// WantedSource is the slice of WantedSync that Discovery needs: the most
// recently synced wanted-missing snapshot, keyed by Lidarr album ID. A
// narrow interface rather than *WantedSync itself so tests can fake it
// without constructing a real WantedSync.
type WantedSource interface {
	Wanted() map[int64]lidarr.WantedAlbum
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
	// MaxCandidateFileRatio rejects a candidate whose file count exceeds the
	// album's known Lidarr track count by more than this multiple (e.g. 2
	// means a candidate offering more than 2x the expected tracks is
	// skipped). Guards against a Soulseek share that dumps an artist's whole
	// discography into one flat folder being mistaken for a single album.
	// Ignored when the album's expected track count is unknown (0), since
	// Lidarr is the final arbiter of import correctness downstream.
	MaxCandidateFileRatio float64
	MaxRetries            int
	BackoffBase           time.Duration
	BackoffCap            time.Duration
	Interval              time.Duration

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
func NewDiscovery(p DiscoveryParams) *Discovery { return &Discovery{p: p} }

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
	return d.searchJob(ctx, jobs[0], now)
}

// searchJob searches for one album, ranks and filters the results, and
// either caches the survivors and advances the job to SELECTING, or backs it
// off (still WANTED) when nothing survives.
func (d *Discovery) searchJob(ctx context.Context, job core.AlbumJob, now time.Time) error {
	album, ok := d.p.WantedSource.Wanted()[job.LidarrAlbumID]
	if !ok {
		// The album is missing from the most recent wanted snapshot. Unlike the
		// legacy engine, Discovery never cancels a job itself - WantedSync owns
		// CANCELLED and will catch this on its own next sync. Just skip this
		// tick and let the job be picked up again once metadata resolves (or
		// cancelled).
		d.log().Info("album missing from wanted snapshot, skipping",
			"album_job", job.ID, "lidarr_album", job.LidarrAlbumID)
		return nil
	}

	query := album.ArtistName + " " + album.Title
	results, err := d.p.Peers.Search(ctx, query, d.p.SearchTimeout)
	if err != nil {
		return err
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
				return err
			}
			query = fallback
		}
	}

	rel, err := d.p.Store.ReliabilityFor(ctx, job.ArtistID, uniqueUsernames(results))
	if err != nil {
		return err
	}
	ranked := d.p.Ranker.Rank(results, rel, now)
	searchDetail := fmt.Sprintf("searched album, query=%q results=%d candidates=%d", query, len(results), len(ranked))
	d.log().Info(searchDetail, "album_job", job.ID, "query", query,
		"results", len(results), "candidates", len(ranked))
	d.recordEvent(ctx, job.ID, core.EventSearch, searchDetail, now)

	// Fetch the album's expected track count once per searchJob call (not per
	// candidate) to size-sanity-check candidates below. total == 0 means
	// Lidarr has no reliable count for this album right now, so the check is
	// skipped entirely rather than risk rejecting a legitimate candidate on
	// bad data. An error here is not counted against the job's retry budget:
	// it aborts this search pass early - log and return nil so the job stays
	// WANTED, untouched, and is retried on a later tick without spending
	// retry budget.
	_, total, err := d.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
	if err != nil {
		d.log().Error("album status failed", "album_job", job.ID, "err", err)
		return nil
	}

	// Candidates are cached per search cycle - the previously-tried-username
	// filter the legacy engine applied at enqueue time is deliberately absent
	// here: a fresh cache is wiped on every retry cycle (see InsertCandidates
	// resetting retries, and ResetJobToWanted deleting prior candidates), so
	// there is no cross-cycle "already tried" state left to consult.
	var survivors []store.NewCandidate
	for _, cand := range ranked {
		if len(survivors) >= d.p.MaxCandidates {
			break
		}
		if total > 0 && float64(len(cand.Files)) > float64(total)*d.p.MaxCandidateFileRatio {
			// A share this oversized for the expected album is almost certainly
			// not a single release (e.g. an artist's whole discography dumped
			// into one flat folder) - skip it rather than cache it.
			detail := fmt.Sprintf("candidate %s file count implausible for album (%d files, expected %d), skipping", cand.Username, len(cand.Files), total)
			d.log().Info(detail, "album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "expected", total)
			d.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
			continue
		}
		if total > 0 && len(cand.Files) < total {
			// A candidate that can't even cover the expected track count is
			// guaranteed to be rejected by the VERIFYING completeness gate after
			// burning a full download cycle - so caching it is guaranteed wasted
			// work. Skip it rather than cache it.
			detail := fmt.Sprintf("candidate %s has fewer files than expected tracks (%d of %d), skipping", cand.Username, len(cand.Files), total)
			d.log().Info(detail, "album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "expected", total)
			d.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
			continue
		}
		survivors = append(survivors, newCandidateFrom(cand))
	}

	if len(survivors) == 0 {
		// The EventSearch event recorded above already carries this cycle's
		// empty outcome (results/candidates counts); nothing further to record.
		d.log().Info("no viable candidates, backing off", "album_job", job.ID)
		return failOrBackoff(ctx, d.p.Store, d.log(), job, d.p.MaxRetries, d.p.BackoffBase, d.p.BackoffCap, false, now)
	}

	if err := d.p.Store.InsertCandidates(ctx, job.ID, survivors, now); err != nil {
		return err
	}
	advanced, err := d.p.Store.AdvanceJobStateFrom(ctx, job.ID, core.StateWanted, core.StateSelecting, now)
	if err != nil {
		return err
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
	return nil
}

// newCandidateFrom converts a ranked matcher.Candidate into the store's
// persisted candidate shape.
func newCandidateFrom(cand matcher.Candidate) store.NewCandidate {
	files := make([]core.CandidateFile, len(cand.Files))
	for i, f := range cand.Files {
		files[i] = core.CandidateFile{Filename: f.Filename, Size: f.Size}
	}
	return store.NewCandidate{Username: cand.Username, Score: cand.Score, Files: files}
}

// uniqueUsernames returns the distinct usernames present in results, in
// first-seen order, used to batch-fetch reliability history for exactly the
// peers a search actually returned rather than one query per candidate.
// Ported from internal/engine/discovery.go.
func uniqueUsernames(results []slskd.Result) []string {
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
