package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/matcher"
	"github.com/hyzr-dev/slusk/internal/store"
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
	// its search cycle (retries=0, empty_searches=0, not_before=NULL) in the
	// same transaction. The reset is guarded on the job still being WANTED -
	// the state this module reads it in - so it silently skips the reset (but
	// still caches the candidates) for a job cancelled underneath this tick.
	InsertCandidates(ctx context.Context, jobID int64, cands []store.NewCandidate, now time.Time) error
	// RejectedCandidates lists the (username, release directory) pairs this
	// job has already tried and failed, across every earlier search cycle.
	// Consulted before caching, since the search itself is deterministic
	// enough to hand back the same failing peers every cycle (issue #317).
	RejectedCandidates(ctx context.Context, jobID int64) ([]store.CandidateRejection, error)
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
	// SetJobEmptySearchBackoff records a search cycle where the Soulseek
	// network returned no raw results at all: it bumps empty_searches and
	// hides the job until notBefore, without touching retries or state (see
	// searchJob's len(results)==0 handling - this never fails the job).
	SetJobEmptySearchBackoff(ctx context.Context, jobID int64, emptySearches int, notBefore time.Time, now time.Time) error
}

// emptySearchRewriteThreshold is how many consecutive no-raw-results cycles
// (job.EmptySearches) a job must accumulate before searchJob tries
// matcher.DropTokenQuery as a second fallback, on top of the existing
// normalized-query fallback. Below the threshold a run of zeros is treated
// as ordinary network noise (see the migration's comment); at or above it,
// a persistently blocked query is worth the extra search.
const emptySearchRewriteThreshold = 2

// DiscoveryMetrics receives discovery metrics. A nil sink is a no-op, so
// Discovery never depends on the observ package directly (same pattern as
// Downloading's MetricsSink).
type DiscoveryMetrics interface {
	IncAlbumReleasesError()
	// IncAlbumTracksError counts one failed Lidarr AlbumTracks call (see
	// searchJob's degrade-not-abort handling below).
	IncAlbumTracksError()
}

// DiscoveryParams configures a Discovery.
type DiscoveryParams struct {
	Store        DiscoveryStore
	Peers        PeerSearcher
	Music        MusicSource
	Ranker       Ranker
	WantedSource WantedSource
	// Metrics receives discovery stats each tick. nil → metrics are not fed
	// (no-op), so Discovery never panics without an observ sink wired.
	Metrics DiscoveryMetrics

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
	// warnedAlbumTracksFailure tracks whether the AlbumTracks degrade-path
	// below has already logged at Warn once. Discovery runs one job per tick
	// on a single goroutine (see runner.go), so this needs no locking. A
	// Lidarr version missing the endpoint would otherwise fail on every
	// wanted album on every tick forever; the first failure is surfaced at
	// Warn (an operator should notice and investigate), every one after that
	// only at Debug (the condition is already known, not new information).
	// Cleared again on the next success, so the throttle covers one failure
	// episode rather than the whole process lifetime.
	warnedAlbumTracksFailure bool
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

// failExcludedSearch handles a search rejected by matchExcludedPhrase
// (core.ErrSearchExcluded, issue #319): the Soulseek server has told every
// well-behaved peer to ignore this exact query, so it is not "nobody has this
// album" and it is not transient - retrying would burn the whole backoff
// budget re-issuing a query that is doomed by construction. maxRetries=0
// makes failOrBackoff fail the job on this first hit (job.Retries+1 is always
// >= 0) instead of backing off. The search cycle still counts as completed
// (it reached a normal conclusion, just an unmatched one), and the excluded-
// search error is deliberately not returned to the caller: propagating it out
// of Tick would misreport a routine, expected outcome as a module failure.
func (d *Discovery) failExcludedSearch(ctx context.Context, job core.AlbumJob, query string, err error, now time.Time) (completed, matched bool, out error) {
	detail := fmt.Sprintf("search excluded by server phrase list: query=%q: %v", query, err)
	d.log().Info(detail, "album_job", job.ID, "query", query)
	d.recordEvent(ctx, job.ID, core.EventSearchExcluded, detail, now)
	if _, ferr := failOrBackoff(ctx, d.p.Store, d.log(), job, 0, d.p.BackoffBase, d.p.BackoffCap, false, detail, now); ferr != nil {
		return false, false, ferr
	}
	return true, false, nil
}

// recordSearchPass best-effort appends one row recording a completed
// Discovery search cycle, for the Overview charts (issue #88). A write
// failure must never block the pipeline, so it is logged at warn level and
// swallowed rather than propagated (same pattern as recordEvent).
func (d *Discovery) recordSearchPass(ctx context.Context, matched bool, now time.Time) {
	// A pass is one search within one tick, so both timestamps use the
	// injected tick time rather than mixing in a wall-clock time.Now().
	pass := core.SearchPass{StartedAt: now, FinishedAt: now, Searched: 1}
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
// matched) rather than aborting early (a manual job reaching Discovery at
// all, failed outright before any of the checks below - see the Source guard
// above; the album missing from the wanted snapshot; an AlbumReleases error -
// checked before any Soulseek search is issued, see below - or a search
// error) - Tick only records a search pass when completed is true. matched
// is true only when the job
// actually advanced to SELECTING: InsertCandidates cached surviving
// candidates AND AdvanceJobStateFrom confirms the job was still WANTED to
// advance from (it can report advanced=false if the job concurrently left
// WANTED, e.g. cancelled by WantedSync between RunnableJobsInState and here).
func (d *Discovery) searchJob(ctx context.Context, job core.AlbumJob, now time.Time) (completed, matched bool, err error) {
	if job.Source == core.SourceManual {
		// A manual job (issue #155) reaching Discovery at all is a broken
		// state: it is created ACTIVE, straight into DOWNLOADING, and never
		// belongs in WANTED. Fail it outright rather than falling through to
		// the wanted-snapshot lookup below - deliberately placed before that
		// lookup, not folded into its own !ok branch, because issue #321 has
		// reason to start setting lidarr_album_id on manual jobs, and then the
		// snapshot lookup would *succeed* and Discovery would search and
		// download someone else's files in the user's name. Guarding on
		// Source closes that door in advance. This also self-heals the
		// zombies already sitting in production WANTED since #58/#155
		// shipped: no migration needed. Not immediately, though -
		// RunnableJobsInState orders WANTED by release_date DESC and a
		// manual job's release_date is always empty, so it sorts last; a
		// zombie is only reached once the real backlog has left WANTED or
		// backed off, not on the very next tick.
		// completed=false, matched=false: this pass is not counted as a
		// search (see Tick).
		detail := "manual job reached Discovery, failing rather than searching"
		d.log().Error(detail, "album_job", job.ID)
		if err := d.p.Store.MarkJobFailed(ctx, job.ID, now); err != nil {
			return false, false, err
		}
		d.recordEvent(ctx, job.ID, core.EventJobFailed, detail, now)
		return false, false, nil
	}

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

	// Fetch the album's releases once per searchJob call to compute the valid
	// track-count band [min, max] across all editions — a candidate matching
	// any real edition's track count is viable, since manual import runs with
	// release switching enabled and Lidarr picks the matching edition itself.
	// A (0,0) band means Lidarr has no usable release data right now, so the
	// filter is skipped entirely rather than risk rejecting a legitimate
	// candidate on bad data. An error here is not counted against the job's
	// retry budget: it aborts this search pass early - log and return nil so
	// the job stays WANTED, untouched, and is retried on a later tick. This
	// cheap local Lidarr call is deliberately fetched before the Soulseek
	// search below (issue #92): a permanently failing Lidarr call aborts the
	// pass here, before any expensive P2P search traffic whose results would
	// only be discarded.
	releases, err := d.p.Music.AlbumReleases(ctx, job.LidarrAlbumID)
	if err != nil {
		if d.p.Metrics != nil {
			d.p.Metrics.IncAlbumReleasesError()
		}
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

	query := album.ArtistName + " " + album.Title
	results, err := d.p.Peers.Search(ctx, query, d.p.SearchTimeout)
	if err != nil {
		if errors.Is(err, core.ErrSearchExcluded) {
			return d.failExcludedSearch(ctx, job, query, err, now)
		}
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
		if fallback := matcher.NormalizeQuery(query); fallback != "" && fallback != query {
			fallbackDetail := fmt.Sprintf("primary search empty, trying normalized query %q", fallback)
			d.log().Info(fallbackDetail, "album_job", job.ID, "query", fallback)
			d.recordEvent(ctx, job.ID, core.EventSearchFallback, fallbackDetail, now)
			results, err = d.p.Peers.Search(ctx, fallback, d.p.SearchTimeout)
			if err != nil {
				if errors.Is(err, core.ErrSearchExcluded) {
					return d.failExcludedSearch(ctx, job, fallback, err, now)
				}
				return false, false, err
			}
			query = fallback
		}

		// Still nothing, and this job has racked up enough consecutive
		// no-raw-results cycles (job.EmptySearches) to suspect it is one of
		// the queries the network silently filters (issue #334) rather than
		// one more instance of ordinary search noise. Try one more search
		// with a single artist token dropped - deterministically rotated by
		// attempt, derived from EmptySearches, so a run of raw-empty cycles
		// tries a different token each time instead of repeating the same
		// doomed rewrite. That only holds while every cycle stays raw-empty:
		// a rewrite that returns raw results but no surviving candidate goes
		// through SetJobBackoff below, which resets EmptySearches to 0, so
		// the next rewrite attempt starts over at attempt 0 rather than
		// continuing the rotation.
		if len(results) == 0 && job.EmptySearches >= emptySearchRewriteThreshold {
			attempt := job.EmptySearches - emptySearchRewriteThreshold
			if rewrite := matcher.DropTokenQuery(album.ArtistName, album.Title, attempt); rewrite != "" && rewrite != query {
				rewriteDetail := fmt.Sprintf("search still empty after %d empty cycles, trying token-dropped query %q", job.EmptySearches, rewrite)
				d.log().Info(rewriteDetail, "album_job", job.ID, "query", rewrite)
				d.recordEvent(ctx, job.ID, core.EventSearchFallback, rewriteDetail, now)
				results, err = d.p.Peers.Search(ctx, rewrite, d.p.SearchTimeout)
				if err != nil {
					if errors.Is(err, core.ErrSearchExcluded) {
						return d.failExcludedSearch(ctx, job, rewrite, err, now)
					}
					return false, false, err
				}
				query = rewrite
			}
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

	// Fetch the album's expected track titles for the relevance gate below,
	// now that there is at least one ranked candidate to check them against -
	// fetching this unconditionally before the search (as originally written)
	// doubled Discovery's Lidarr call rate for every job whose search came
	// back empty, for no benefit. This DEGRADES rather than aborts on error -
	// unlike AlbumReleases above, which is load-bearing for the track-count
	// band. Making this second Lidarr endpoint a hard dependency of all
	// discovery would mean a 404 on some deployed Lidarr version (this
	// endpoint's shape is unverified, see lidarr.Client.AlbumTracks) silently
	// stops slusk searching for everything. The directory-only half of the
	// relevance gate still fixes issue #316 on its own, so losing
	// track-title evidence only makes the gate slightly less precise, not
	// inert.
	var trackTitles []string
	if len(ranked) > 0 {
		tracks, err := d.p.Music.AlbumTracks(ctx, job.LidarrAlbumID)
		if err != nil {
			if d.p.Metrics != nil {
				d.p.Metrics.IncAlbumTracksError()
			}
			logFn := d.log().Debug
			if !d.warnedAlbumTracksFailure {
				logFn = d.log().Warn
				d.warnedAlbumTracksFailure = true
			}
			logFn("album tracks failed, relevance gate degrades to directory check",
				"album_job", job.ID, "err", err)
		} else {
			// Arm the Warn again: the throttle is per failure episode, not per
			// process. Without this a transient blip permanently demotes every
			// later outage - including an unrelated one weeks on - to Debug.
			d.warnedAlbumTracksFailure = false
			trackTitles = make([]string, len(tracks))
			for i, tr := range tracks {
				trackTitles[i] = tr.Title
			}
		}
	}

	// Candidates are cached per search cycle, and ResetJobToWanted wipes that
	// cache on every retry - but the search producing them is deterministic
	// enough that the next cycle hands back the same peers, including the ones
	// that just failed. candidate_rejections is the cross-cycle memory that
	// survives the wipe (issue #317); without consulting it here, a job whose
	// search is consistently wrong pays MaxRetries × MaxCandidates full
	// downloads of the same bad files before it finally fails.
	rejections, err := d.p.Store.RejectedCandidates(ctx, job.ID)
	if err != nil {
		return false, false, err
	}
	rejected := make(map[candidateKey]struct{}, len(rejections))
	for _, r := range rejections {
		rejected[candidateKey{Username: r.Username, ReleaseDir: r.ReleaseDir}] = struct{}{}
	}

	var survivors []store.NewCandidate
	var tooManyTracks, tooFewTracks, irrelevant, alreadyFailed int
	for _, cand := range ranked {
		if len(survivors) >= d.p.MaxCandidates {
			break
		}
		// Checked first: this candidate has already been downloaded in full and
		// failed for this job, so no amount of evidence from the cheaper filters
		// below could make it worth trying again.
		if key, ok := candidateKeyOf(cand); ok {
			if _, seen := rejected[key]; seen {
				detail := fmt.Sprintf("candidate %s already failed for this job (%s), skipping", cand.Username, key.ReleaseDir)
				d.log().Debug(detail, "album_job", job.ID, "user", cand.Username, "release_dir", key.ReleaseDir)
				alreadyFailed++
				continue
			}
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
		// Placed after the (cheaper) track-count checks so those rejection
		// reasons keep taking precedence in the counts below (issue #316):
		// Soulseek search is a token-AND over a peer's whole shared path, so a
		// query for "The Absence The Absence" network-matches
		// "Kansas\The Absence Of Presence (2020)\..." - a valid hit for the
		// wrong album. Neither the track-count band above nor matcher.Rank's
		// scoring has any notion of "wrong album"; this is the only check
		// that does.
		if v := matcher.CheckRelevance(matcher.RelevanceInput{
			ArtistName: album.ArtistName, AlbumTitle: album.Title,
			TrackTitles: trackTitles, Files: filenamesOf(cand.Files),
		}); !v.Match {
			detail := fmt.Sprintf("candidate %s does not match the requested album (%s), skipping", cand.Username, v.Reason)
			// "source" (which evidence decided - track titles vs directory),
			// not a repeat of Reason, which detail above already carries:
			// matches the neighbouring rejection branches' pattern of logging
			// structured values, not a copy of the message (see v.Source's
			// doc comment).
			// .String() explicitly: production wires slog's JSON handler, which
			// marshals the int-typed RelevanceSource as a bare number and never
			// consults Stringer. Tests use the text handler, which does - so
			// dropping this reads correctly in every test and logs "source":2
			// in the only place that matters.
			d.log().Debug(detail, "album_job", job.ID, "user", cand.Username, "source", v.Source.String())
			irrelevant++
			continue
		}
		survivors = append(survivors, newCandidateFrom(cand))
	}

	if n := tooManyTracks + tooFewTracks + irrelevant + alreadyFailed; n > 0 {
		detail := fmt.Sprintf("rejected %d candidates: %d above maximum track count, %d below minimum track count, %d not matching the requested album, %d already failed for this job",
			n, tooManyTracks, tooFewTracks, irrelevant, alreadyFailed)
		d.log().Info(detail, "album_job", job.ID, "rejected", n,
			"above_max_tracks", tooManyTracks, "below_min_tracks", tooFewTracks,
			"irrelevant", irrelevant, "already_failed", alreadyFailed)
		d.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
	}

	if len(survivors) == 0 {
		if len(results) == 0 {
			// The network answered nothing at all this cycle, even after the
			// normalized-query and (if eligible) token-dropped fallbacks
			// above - not "peers answered but every candidate was rejected",
			// which is the branch below. A single empty search is weak
			// evidence (issue #334, see migration 0012's comment for the
			// measurement behind that claim). So this does NOT touch the
			// retry budget and NEVER fails the job - it backs off on its own
			// empty_searches curve and stays WANTED forever, retried at
			// backoff_cap intervals, which is the deliberate answer for a
			// genuinely unanswerable query rather than a fabricated
			// terminal state.
			empty := job.EmptySearches + 1
			notBefore := now.Add(nextBackoff(empty, d.p.BackoffBase, d.p.BackoffCap))
			d.log().Info("no raw results, backing off empty-search streak without touching retries",
				"album_job", job.ID, "empty_searches", empty)
			if err := d.p.Store.SetJobEmptySearchBackoff(ctx, job.ID, empty, notBefore, now); err != nil {
				return false, false, err
			}
			return true, false, nil
		}
		// The EventSearch event recorded above already carries this cycle's
		// empty outcome (results/candidates counts); nothing further to record.
		// The same counts go in as failOrBackoff's reason so a terminal
		// job_failed explains itself without a reader walking back to the
		// EventSearch row (issue #318).
		reason := fmt.Sprintf("no viable candidates: all %d search results rejected by filtering", len(results))
		d.log().Info(reason+", backing off", "album_job", job.ID)
		// The terminal-failure signal is discarded on purpose: a Discovery-
		// terminal job is in WANTED with its candidates and transfers already
		// gone, so there is nothing to derive a leftover download folder from.
		_, err := failOrBackoff(ctx, d.p.Store, d.log(), job, d.p.MaxRetries, d.p.BackoffBase, d.p.BackoffCap, false, reason, now)
		return true, false, err
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
		// engine. This search cycle did not actually match anything usable, so
		// matched must be false even though InsertCandidates succeeded.
		d.log().Info("job left WANTED before candidates could be applied, leaving candidates in place",
			"album_job", job.ID)
	}
	return true, advanced, nil
}

// candidateKey identifies "the same candidate" across search cycles: one peer's
// one release directory. Username alone is too blunt - the same peer may well
// share the right album in another directory - and this pair is exactly what
// matcher.Rank groups search results on, so one rejection matches one candidate.
type candidateKey struct {
	Username   string
	ReleaseDir string
}

// candidateKeyOf derives a ranked candidate's key from its first file, matching
// how the store derives it when recording a rejection. ok is false for a
// candidate with no files, which has no directory to key on - such a candidate
// is never filtered rather than being matched against an empty key, which would
// collapse every unkeyable candidate onto one another.
func candidateKeyOf(cand core.RankedCandidate) (candidateKey, bool) {
	if len(cand.Files) == 0 {
		return candidateKey{}, false
	}
	return candidateKey{Username: cand.Username, ReleaseDir: matcher.ReleaseDir(cand.Files[0].Filename)}, true
}

// filenamesOf extracts a candidate's filenames for matcher.CheckRelevance.
func filenamesOf(files []core.SearchResult) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Filename
	}
	return out
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
