package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// ImportingStore is the slice of the store Importing needs. Declared here (Go
// style: the consumer owns the interface) rather than reusing the legacy
// engine's JobStore, so the pipeline package has no dependency on
// internal/engine. The concrete *store.Store satisfies it implicitly.
type ImportingStore interface {
	// RunnableJobsInState selects this tick's batch of IMPORTING jobs, bounded
	// by MaxActive (the DOWNLOADING+IMPORTING ceiling), same rationale as
	// Downloading's resolve/top-up phases.
	RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
	// ActiveCandidate returns an IMPORTING job's single active candidate; its
	// ImportSubmittedAt gates verify (nil) vs confirm (set).
	ActiveCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error)
	// TransfersForCandidate supplies the filenames AlbumFolder needs.
	TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)
	// MarkImportSubmitted records that ExecuteManualImport has been called,
	// switching the candidate from verify to confirm on the next tick.
	MarkImportSubmitted(ctx context.Context, candidateID int64, now time.Time) error
	// SucceedCandidateAndAdvance atomically marks the candidate SUCCEEDED and
	// advances the job IMPORTING→DONE (import confirmed, or the idempotent
	// already-imported path) in one tx.
	SucceedCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error)
	// FailCandidateAndAdvance atomically marks the candidate FAILED and advances
	// the job IMPORTING→SELECTING (rejected, incomplete, stuck, or unconfirmed
	// past ImportConfirmTimeout) in one tx, so a job is never left in IMPORTING
	// with no ACTIVE candidate.
	FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error)
	// RejectCandidateAndAdvance is FailCandidateAndAdvance for a failure that
	// is the candidate's own content's fault (Lidarr rejected the files, or
	// they cover no known edition): it additionally records the candidate in
	// the job's rejection history, so a later search cycle does not re-cache
	// and re-download the same files (issue #317). A failure that says nothing
	// about the content - a stuck import, a peer dropping mid-transfer - must
	// use FailCandidateAndAdvance instead.
	RejectCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error)
	// SetJobNotBefore hides the job from RunnableJobsInState until notBefore
	// without touching retries or updated_at, so a verify-phase retry cooldown
	// does not reset escalateIfStuck's StuckAfter clock (keyed on updated_at).
	SetJobNotBefore(ctx context.Context, jobID int64, notBefore time.Time) error
	// RecordAttemptOutcome writes a candidate's terminal success/fail outcome to
	// the peer reliability tables (best-effort).
	RecordAttemptOutcome(ctx context.Context, artistID int64, username string, success bool, now time.Time) error
	// AddJobEvent appends one row to a job's audit trail.
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
	// DownloadFoldersForJob and MarkDownloadFolderCleaned are the register the
	// two cleanup helpers read instead of re-deriving the folder from surviving
	// transfer filenames (issue #314).
	DownloadFoldersForJob(ctx context.Context, jobID int64) ([]string, error)
	MarkDownloadFolderCleaned(ctx context.Context, jobID int64, leaf string, now time.Time) error
}

// ImportingParams configures an Importing.
type ImportingParams struct {
	Store  ImportingStore
	Music  MusicSource
	Peers  FolderCleaner // DeleteDownloadFolder only, for cleanupFolder
	Logger *slog.Logger

	CompleteDir          string
	MaxActive            int
	StuckAfter           time.Duration
	ImportConfirmTimeout time.Duration
	Interval             time.Duration
	// RetryCooldown is how long a job waits before re-attempting a failed
	// verify-phase Lidarr call (ManualImportCandidates/AlbumStatus) on the
	// same folder, instead of retrying every Interval until StuckAfter. Zero
	// disables the cooldown (retry next tick, the pre-cooldown behavior).
	RetryCooldown time.Duration
}

// Importing owns every IMPORTING-state job. IMPORTING replaces the legacy
// engine's separate VERIFYING and IMPORTING states with one state and two
// phases, keyed on the active candidate's ImportSubmittedAt (nil → verify,
// set → confirm) rather than distinct job states:
//
//   - Verify (ImportSubmittedAt is NULL): ask Lidarr what it would import from
//     the album folder and decide whether to import at all. Ported from the
//     legacy Discoverer.advanceImporting (engine/discovery.go:677-774).
//   - Confirm (ImportSubmittedAt is set): Lidarr's ManualImport command is
//     asynchronous, so rather than trusting its HTTP response, poll the
//     album's completeness directly until it completes or the confirm timeout
//     elapses. Ported from the legacy Discoverer.confirmImports
//     (engine/discovery.go:845-894).
//
// Both phases fetch this tick's batch from one RunnableJobsInState(IMPORTING)
// call (there is no per-phase state to filter on), then dispatch each job by
// its active candidate's ImportSubmittedAt.
type Importing struct {
	p ImportingParams
}

// NewImporting constructs an Importing.
func NewImporting(p ImportingParams) *Importing {
	if p.Logger != nil {
		p.Logger = p.Logger.With("module", "importing")
	}
	return &Importing{p: p}
}

// Name identifies this module in logs and Health().
func (m *Importing) Name() string { return "importing" }

// Interval is how often this module ticks.
func (m *Importing) Interval() time.Duration { return m.p.Interval }

func (m *Importing) log() *slog.Logger {
	if m.p.Logger != nil {
		return m.p.Logger
	}
	return slog.Default()
}

// recordEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it is
// logged at warn level and swallowed rather than propagated (same pattern as
// Downloading.recordEvent).
func (m *Importing) recordEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) {
	if err := m.p.Store.AddJobEvent(ctx, jobID, event, detail, now); err != nil {
		m.log().Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
	}
}

// recordOutcome writes a candidate's terminal success/fail outcome to the peer
// reliability tables (see Store.RecordAttemptOutcome). Best-effort: a failure
// to record must not block the job's own state transition, so it is logged
// and swallowed rather than propagated. Ported from the legacy
// Discoverer.recordOutcome (engine/discovery.go:473-478).
func (m *Importing) recordOutcome(ctx context.Context, job core.AlbumJob, username string, success bool, now time.Time) {
	if err := m.p.Store.RecordAttemptOutcome(ctx, job.ArtistID, username, success, now); err != nil {
		m.log().Error("record peer reliability outcome failed",
			"album_job", job.ID, "user", username, "success", success, "err", err)
	}
}

// Tick dispatches every runnable IMPORTING job to the verify or confirm phase
// depending on its active candidate's ImportSubmittedAt.
func (m *Importing) Tick(ctx context.Context, now time.Time) error {
	jobs, err := m.p.Store.RunnableJobsInState(ctx, core.StateImporting, now, m.p.MaxActive)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		cand, found, err := m.p.Store.ActiveCandidate(ctx, job.ID)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		// A manual job carries no lidarr_album_id, so its album is resolved
		// from AlbumMBID here, once per tick, before either phase can reach
		// an AlbumStatus call. Resolving in Tick rather than inside verify
		// covers confirm too, which polls AlbumStatus on every tick of its
		// own and would otherwise be the one path still able to ask for
		// album 0.
		//
		// The resolved id is deliberately NOT written back to the job row.
		// album_jobs.lidarr_album_id means "this job came from Lidarr's
		// wanted list", so writing one onto a manual job puts two rows in the
		// table claiming the same album - which the partial unique index
		// cannot prevent, since it only covers source = 'lidarr'. WantedSync's
		// predicates survive that today because #369 made every one of them
		// filter on source explicitly, but the ambiguity is still real for
		// anything that reads the column as an album identity.
		// Re-resolving costs one library lookup per tick on a job that is
		// importing anyway, and in exchange a Lidarr album that was deleted
		// and re-added is picked up on the next tick instead of stranding the
		// job on a stale id.
		resolved, ok, err := m.resolveAlbumID(ctx, job, cand, now)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		job.LidarrAlbumID = resolved
		if cand.ImportSubmittedAt == nil {
			if err := m.verify(ctx, job, cand, now); err != nil {
				return err
			}
			continue
		}
		if err := m.confirm(ctx, job, cand, now); err != nil {
			return err
		}
	}
	return nil
}

// failCandidate is the shared "reject like a failed download" path used by
// both the rejection and incomplete-coverage cases in verify: cleanup, record
// outcome, then atomically fail the candidate and return the job to SELECTING.
// The candidate-fail + job-advance commit together or not at all, so the job is
// never stranded in IMPORTING with no ACTIVE candidate. No cooldown is set -
// other candidates usually remain cached, so the next SELECTING tick tries one
// immediately. The best-effort cleanup/outcome errors are logged inside their
// helpers; only the combined store call's error is propagated.
//
// A manual job's folder is never cleaned up (issue #59). cleanupFolder is a
// recursive delete, and for a Lidarr-sourced job that is right: the download
// was slusk's own attempt, another candidate will be tried, and the
// rejected files are cruft. A manual job has no next candidate - Selecting
// marks it FAILED - and its files are the thing the user explicitly asked
// for. Deleting them turns "Lidarr would not import this" into silent data
// loss, which is the one outcome a manual download must never produce. The
// commonest way to get here is the coverage gate: someone picks six tracks
// of a twelve-track album, so importable < MinTrackCount and the candidate
// is rejected. The files stay; the job still reports why in its events.
//
// contentFault says whether the failure was the candidate's own files' doing,
// which is what earns it a permanent place in the job's rejection history
// (issue #317). Lidarr refusing the files and a folder covering no known
// edition qualify; an import stuck past its timeout does not - that is Lidarr's
// state, not this peer's, and it would recur for every candidate in turn.
func (m *Importing) failCandidate(ctx context.Context, job core.AlbumJob, cand core.Candidate, reason string, contentFault bool, now time.Time) error {
	if job.Source != core.SourceManual {
		cleanupFolder(ctx, m.p.Peers, m.p.Store, m.log(), job.ID, now)
	}
	m.recordOutcome(ctx, job, cand.Username, false, now)
	advance := m.p.Store.FailCandidateAndAdvance
	if contentFault {
		advance = m.p.Store.RejectCandidateAndAdvance
	}
	if _, err := advance(ctx, cand.ID, job.ID, reason, core.StateImporting, core.StateSelecting, now); err != nil {
		return err
	}
	return nil
}

// verify is the verify-phase gate (ImportSubmittedAt is NULL): it asks Lidarr
// what it would import from the album folder and decides whether to import at
// all. Files Lidarr assigned a real track ID are imported even if it also
// stamped a folder-level rejection on them (e.g. "Has unmatched tracks", which
// Lidarr applies to every file in a non-bijective folder); a candidate with no
// matched file at all fails outright (SELECTING, next candidate tried
// immediately - no cooldown). A clean candidate that cannot cover the smallest
// valid edition is also failed, rather than importing a partial album. A
// clean, complete candidate is imported and ImportSubmittedAt is set so the
// next tick's confirm phase can confirm Lidarr accepted it.
//
// Ported from the legacy Discoverer.advanceImporting's per-job body
// (engine/discovery.go:677-774).
func (m *Importing) verify(ctx context.Context, job core.AlbumJob, cand core.Candidate, now time.Time) error {
	transfers, err := m.p.Store.TransfersForCandidate(ctx, cand.ID)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(transfers))
	for _, t := range transfers {
		names = append(names, t.Filename)
	}

	// job.LidarrAlbumID is already resolved by Tick (see resolveAlbumID), so
	// it is non-zero here for every job, manual or not.
	folder := AlbumFolder(m.p.CompleteDir, names)
	if folder != m.p.CompleteDir {
		// Best-effort dedup before Lidarr scans the folder: a messy share can
		// contain the same track twice (mixed formats, stray copies), which
		// makes Lidarr's matching ambiguous. Skipped when AlbumFolder fell
		// back to the download root itself — deduping there could remove
		// other albums' files. A dedup failure (e.g. folder already imported
		// and gone) must not block verify; the scan below copes either way.
		if removed, err := dedupAlbumFolder(m.log(), folder); err != nil {
			m.log().Warn("dedup album folder failed", "album_job", job.ID, "folder", folder, "err", err)
		} else if len(removed) > 0 {
			detail := fmt.Sprintf("removed %d duplicate track file(s) before import", len(removed))
			m.log().Info(detail, "album_job", job.ID, "folder", folder, "removed", removed)
			m.recordEvent(ctx, job.ID, core.EventDedup, detail, now)
		}
	}
	items, err := m.p.Music.ManualImportCandidates(ctx, folder)
	if err != nil {
		m.log().Error("manual import candidates failed", "album_job", job.ID, "folder", folder, "err", err)
		return m.escalateIfStuck(ctx, job, cand, "import candidates failed", now)
	}
	if len(items) == 0 {
		// An empty Lidarr preview only proves the import already happened when
		// the exact local folder Lidarr scanned is also absent or empty. Lidarr
		// may otherwise return an empty preview when it cannot see the supplied
		// path (for example, because of a container path mismatch).
		entries, readErr := os.ReadDir(folder)
		if readErr != nil && !os.IsNotExist(readErr) {
			m.log().Error("inspect folder after empty import candidates failed",
				"album_job", job.ID, "folder", folder, "err", readErr)
			return m.escalateIfStuck(ctx, job, cand, "inspect folder after empty import candidates failed", now)
		}
		if readErr == nil && len(entries) > 0 {
			m.log().Error("empty import candidates for non-empty folder",
				"album_job", job.ID, "folder", folder, "entries", len(entries))
			return m.escalateIfStuck(ctx, job, cand, "empty import candidates for non-empty folder", now)
		}

		// The folder is absent or actually empty, consistent with a crash
		// between a prior successful ExecuteManualImport and this state write.
		// Treat it as done so verify remains idempotent across restarts.
		emptyDetail := fmt.Sprintf("empty folder treated as already imported (folder %s)", folder)
		m.log().Info(emptyDetail, "album_job", job.ID, "folder", folder)
		m.recordEvent(ctx, job.ID, core.EventAttemptSucceeded, emptyDetail, now)
		m.recordOutcome(ctx, job, cand.Username, true, now)
		if _, err := m.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateImporting, core.StateDone, now); err != nil {
			return err
		}
		cleanupCompletedFolder(ctx, m.p.Store, m.log(), job.ID, m.p.CompleteDir, now)
		return nil
	}
	// Classify by TrackIDs rather than Lidarr's per-file Importable flag:
	// Lidarr stamps folder-level rejections like "Has unmatched tracks" on
	// every file in a folder that isn't a perfect bijection against the
	// release — including files it did match to a track. A file with one or
	// more real track IDs was matched and is importable; only files with no
	// track ID at all are genuinely unmatched.
	var importable []core.ImportItem
	var rejections []string
	for _, it := range items {
		if len(it.TrackIDs) > 0 {
			importable = append(importable, it)
		} else {
			rejections = append(rejections, it.Rejections...)
		}
	}
	if len(importable) == 0 {
		// Nothing matched at all. This usually means the candidate's files are
		// bad, but Lidarr also rejects every file with no TrackIDs when the
		// release is already fully present in the library (e.g. imported by a
		// previous candidate, or by an out-of-band Lidarr action) — there is
		// nothing left for it to match against. Failing the candidate in that
		// case just burns through every remaining candidate for an album that
		// was never actually missing (issue #280), so check AlbumStatus before
		// giving up.
		complete, err := m.albumAlreadyComplete(ctx, job)
		if err != nil {
			m.log().Warn("album status check before import-rejected failure failed", "album_job", job.ID, "err", err)
		} else if complete {
			alreadyDetail := fmt.Sprintf("import rejected but album already complete in Lidarr (folder %s): %s", folder, strings.Join(rejections, "; "))
			m.log().Info(alreadyDetail, "album_job", job.ID, "folder", folder, "reasons", rejections)
			m.recordEvent(ctx, job.ID, core.EventAttemptSucceeded, alreadyDetail, now)
			if _, err := m.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateImporting, core.StateDone, now); err != nil {
				return err
			}
			cleanupFolder(ctx, m.p.Peers, m.p.Store, m.log(), job.ID, now)
			return nil
		}
		// Other candidates usually remain, so the next SELECTING tick tries one
		// immediately — no cooldown.
		rejectedDetail := fmt.Sprintf("import rejected (folder %s): %s", folder, strings.Join(rejections, "; "))
		m.log().Info(rejectedDetail, "album_job", job.ID, "folder", folder, "reasons", rejections)
		m.recordEvent(ctx, job.ID, core.EventImportRejected, rejectedDetail, now)
		return m.failCandidate(ctx, job, cand, "import rejected", true, now)
	}
	if len(rejections) > 0 {
		// Unmatched extras don't fail the candidate — the coverage gate below
		// decides completeness. They are left behind in the download folder
		// (cleanupCompletedFolder already leaves non-empty folders in place).
		m.log().Info("ignoring unmatched files in album folder",
			"album_job", job.ID, "folder", folder, "unmatched", len(items)-len(importable), "reasons", rejections)
	}
	// Completeness is judged against the smallest valid edition
	// (MinTrackCount, cached by Discovery from Lidarr's release list): with
	// release switching enabled, a candidate matching any real edition is a
	// full album. When the band was never cached (0) — every manual job, since
	// only Discovery caches it, plus any job searched before the band existed
	// — read the release list live rather than asking AlbumStatus.
	//
	// AlbumStatus is the wrong question here twice over. It reports the
	// *currently selected* release's track count, so a legitimate download of
	// one edition is measured against a different one: a real manual job was
	// rejected "covered 10/21" for a ten-track album whose selected release
	// was the twenty-one-track 2xCD, and Lidarr then never got to switch
	// release, because switching happens during the import this gate refused
	// to start. And it reports 0 for an unmonitored album with no files yet,
	// which would turn the gate into a no-op instead of a stricter check.
	// The release list answers both correctly and is unaffected by monitoring.
	minRequired := job.MinTrackCount
	if minRequired == 0 {
		releases, err := m.p.Music.AlbumReleases(ctx, job.LidarrAlbumID)
		if err != nil {
			m.log().Error("album releases lookup failed", "album_job", job.ID, "err", err)
			return m.escalateIfStuck(ctx, job, cand, "album releases lookup failed", now)
		}
		minRequired, _ = trackBand(releases)
		if minRequired == 0 {
			// No release carries a usable track count, so there is no
			// expectation to measure against. Say so rather than letting a
			// gate that could not be evaluated read as a gate that passed;
			// Lidarr still has the final word on the import itself.
			m.log().Warn("no usable track count on any release, skipping the coverage gate",
				"album_job", job.ID, "releases", len(releases))
		}
	}
	if coverage(importable) < minRequired {
		// A source that can't complete any valid edition is rejected outright
		// rather than partially imported, to keep the library free of half
		// albums. Other candidates usually remain, so use the no-cooldown
		// fail path.
		incompleteDetail := fmt.Sprintf("incomplete download, rejecting (folder %s, covered %d/%d)", folder, coverage(importable), minRequired)
		m.log().Info(incompleteDetail, "album_job", job.ID, "folder", folder,
			"covered", coverage(importable), "required", minRequired)
		m.recordEvent(ctx, job.ID, core.EventImportRejected, incompleteDetail, now)
		return m.failCandidate(ctx, job, cand, "incomplete download", true, now)
	}
	if err := m.p.Music.ExecuteManualImport(ctx, importable); err != nil {
		// Legacy propagated this error from the whole pass (advanceImporting
		// returned it directly); mirrored here by returning it from Tick, which
		// halts the rest of this tick's jobs too.
		m.log().Error("execute manual import failed", "album_job", job.ID, "err", err)
		return err
	}
	importOKDetail := fmt.Sprintf("import executed, awaiting confirmation (%d files)", len(importable))
	m.log().Info(importOKDetail, "album_job", job.ID, "files", len(importable))
	m.recordEvent(ctx, job.ID, core.EventImportOK, importOKDetail, now)
	// Crash between ExecuteManualImport and MarkImportSubmitted re-runs verify
	// on the next tick; the empty-folder path above absorbs it (Lidarr already
	// imported the files, so ManualImportCandidates finds nothing left).
	if err := m.p.Store.MarkImportSubmitted(ctx, cand.ID, now); err != nil {
		m.log().Error("mark import submitted failed", "candidate", cand.ID, "err", err)
	}
	return nil
}

// resolveAlbumID returns the Lidarr album id to use for this job on this tick.
// A Lidarr-sourced job already carries one. A manual job (issue #59) carries
// none and is resolved from its AlbumMBID, which must happen before either
// phase can make an AlbumStatus call - AlbumStatus(0) errors on every tick,
// which is the defect #59 exists to fix.
//
// The second return is false when this tick's work on the job is already
// done: it was routed to the terminal NOT_IMPORTED (nothing to import into),
// or the Lidarr lookup failed and escalateIfStuck cooled the job down to try
// again next tick.
func (m *Importing) resolveAlbumID(ctx context.Context, job core.AlbumJob, cand core.Candidate, now time.Time) (int64, bool, error) {
	if job.LidarrAlbumID != 0 {
		return job.LidarrAlbumID, true, nil
	}
	if job.AlbumMBID == "" {
		// Defensive: Downloading's allDone routing (downloading.go) should
		// already have sent an unidentified manual job straight to
		// NOT_IMPORTED without ever reaching IMPORTING. If it somehow did
		// anyway, this must still never fall through to AlbumStatus(0).
		return 0, false, m.routeNotImported(ctx, job, cand, "no album identified for import", now)
	}
	album, found, err := m.p.Music.AlbumByForeignID(ctx, job.AlbumMBID)
	if err != nil {
		m.log().Error("resolve manual job album failed", "album_job", job.ID, "album_mbid", job.AlbumMBID, "err", err)
		// Only a manual job can reach here (a Lidarr job returned above), and
		// failCandidate never cleans up a manual job's folder - see its comment.
		return 0, false, m.escalateIfStuck(ctx, job, cand, "resolve album failed", now)
	}
	if !found {
		detail := fmt.Sprintf("identified release group %s is not in Lidarr's library", job.AlbumMBID)
		return 0, false, m.routeNotImported(ctx, job, cand, detail, now)
	}
	return album.ID, true, nil
}

// routeNotImported advances job to the terminal NOT_IMPORTED (issue #59): the
// download succeeded but there is no Lidarr album to import into. No cleanup
// runs - the downloaded files are the deliverable, exactly like Downloading's
// own allDone routing for the same case (downloading.go).
//
// The candidate is marked SUCCEEDED, not failed, and the peer is credited: it
// delivered every file it was asked for. Nothing about this outcome is the
// peer's doing, and leaving the candidate ACTIVE on a terminal job would both
// strand it in ActiveCandidate forever and make the job detail view report an
// attempt still "In progress" on a job that has finished.
func (m *Importing) routeNotImported(ctx context.Context, job core.AlbumJob, cand core.Candidate, detail string, now time.Time) error {
	m.log().Info(detail, "album_job", job.ID)
	m.recordEvent(ctx, job.ID, core.EventNotImported, detail, now)
	m.recordOutcome(ctx, job, cand.Username, true, now)
	if _, err := m.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateImporting, core.StateNotImported, now); err != nil {
		return err
	}
	return nil
}

// escalateIfStuck fails and returns a job to SELECTING once it has been stuck
// (no state change) longer than StuckAfter, so a job whose Lidarr call
// (ManualImportCandidates or AlbumStatus) keeps erroring every tick forever -
// e.g. Lidarr repeatedly timing out on a broken folder - does not stay stuck
// in the verify phase indefinitely, starving every other job's tick. Called
// after logging the triggering error; within the timeout it is a no-op, so
// the job simply gets retried next tick. Verify phase only - confirm has its
// own ImportConfirmTimeout.
//
// Ported from the legacy Discoverer.escalateIfStuck (engine/discovery.go:783-798).
// Within the timeout it is a no-op (returns nil); past it, it propagates the
// atomic fail path's error.
func (m *Importing) escalateIfStuck(ctx context.Context, job core.AlbumJob, cand core.Candidate, reason string, now time.Time) error {
	if now.Sub(job.UpdatedAt) <= m.p.StuckAfter {
		// Not stuck yet: cool the job down instead of re-firing the same slow
		// Lidarr call (typically a large-folder scan timing out) on the very
		// next tick. Best-effort - a failed write just means the old
		// retry-next-tick behavior for one round. SetJobNotBefore leaves
		// updated_at alone, so the StuckAfter clock above keeps running.
		if m.p.RetryCooldown > 0 {
			if err := m.p.Store.SetJobNotBefore(ctx, job.ID, now.Add(m.p.RetryCooldown)); err != nil {
				m.log().Warn("set import retry cooldown failed", "album_job", job.ID, "err", err)
			}
		}
		return nil
	}
	stuckDetail := fmt.Sprintf("importing stuck past timeout (%s)", reason)
	m.log().Info(stuckDetail, "album_job", job.ID, "reason", reason)
	m.recordEvent(ctx, job.ID, core.EventAttemptFailed, stuckDetail, now)
	return m.failCandidate(ctx, job, cand, reason, false, now)
}

// albumAlreadyComplete reports whether Lidarr's library already holds every
// track of job's album, independent of what the current candidate
// contributed. total==0 (AlbumStatus couldn't determine a canonical total,
// e.g. a stale or unknown album) never counts as complete — only a positive
// total that present has met or exceeded does.
func (m *Importing) albumAlreadyComplete(ctx context.Context, job core.AlbumJob) (bool, error) {
	present, total, err := m.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
	if err != nil {
		return false, err
	}
	return total > 0 && present >= total, nil
}

// coverage counts the distinct Lidarr track IDs covered by importable, used to
// judge whether a candidate can complete a release's full track count. Ported
// verbatim from the legacy engine (engine/discovery.go:829-837).
func coverage(importable []core.ImportItem) int {
	seen := make(map[int64]struct{})
	for _, it := range importable {
		for _, id := range it.TrackIDs {
			seen[id] = struct{}{}
		}
	}
	return len(seen)
}

// confirm is the confirm-phase poll (ImportSubmittedAt is set): Lidarr's
// ManualImport command is asynchronous, so rather than trusting the command's
// HTTP response, this polls the album's completeness directly. A release that
// becomes complete succeeds the candidate and advances the job to DONE; one
// that stays incomplete past ImportConfirmTimeout is failed (SELECTING, next
// candidate tried immediately) so a stuck or dropped async import doesn't
// leave the job stranded in IMPORTING forever. Unlike verify's fail path, no
// cleanupFolder runs here - the files were imported/moved by Lidarr, not left
// behind by a failed download.
//
// Ported from the legacy Discoverer.confirmImports's per-job body
// (engine/discovery.go:845-894). Timeout deviation from legacy (binding,
// per the task brief): legacy keyed the timeout on job.UpdatedAt; the
// pipeline keys it on cand.ImportSubmittedAt, since ImportSubmittedAt is the
// actual start of the async wait and job.UpdatedAt could be bumped by
// unrelated writes.
func (m *Importing) confirm(ctx context.Context, job core.AlbumJob, cand core.Candidate, now time.Time) error {
	present, total, err := m.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
	if err != nil {
		m.log().Error("album status failed", "album_job", job.ID, "err", err)
		if now.Sub(*cand.ImportSubmittedAt) > m.p.ImportConfirmTimeout {
			return m.failUnconfirmed(ctx, job, cand, "import not confirmed in time (album status check failing), cooling down", now)
		}
		return nil
	}
	// total > 0 for the same reason albumAlreadyComplete requires it, and it
	// is not hypothetical: Lidarr reports statistics.trackCount as 0 for an
	// album that is unmonitored and has no files yet, which is precisely the
	// state a manual job's album sits in between submitting the import and
	// Lidarr finishing it. Without this guard that reads as 0 >= 0, and the
	// job would announce "import confirmed, completed (0/0 present)", mark
	// itself DONE and delete the download folder having imported nothing.
	// A total that stays 0 is not success, it is an answer we do not have
	// yet — let ImportConfirmTimeout below decide when to give up.
	if total > 0 && present >= total {
		confirmedDetail := fmt.Sprintf("import confirmed, completed (%d/%d present)", present, total)
		m.log().Info(confirmedDetail, "album_job", job.ID)
		m.recordEvent(ctx, job.ID, core.EventAttemptSucceeded, confirmedDetail, now)
		m.recordOutcome(ctx, job, cand.Username, true, now)
		if _, err := m.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateImporting, core.StateDone, now); err != nil {
			return err
		}
		cleanupCompletedFolder(ctx, m.p.Store, m.log(), job.ID, m.p.CompleteDir, now)
		return nil
	}
	if now.Sub(*cand.ImportSubmittedAt) > m.p.ImportConfirmTimeout {
		notConfirmedDetail := fmt.Sprintf("import not confirmed in time (%d/%d present), cooling down", present, total)
		return m.failUnconfirmed(ctx, job, cand, notConfirmedDetail, now)
	}
	return nil
}

// failUnconfirmed is confirm's shared "import not confirmed" fail path: unlike
// verify's failCandidate, it does not cleanupFolder (the files were
// imported/moved by Lidarr, not left behind by a failed download - the legacy
// confirmImports didn't clean up here either).
func (m *Importing) failUnconfirmed(ctx context.Context, job core.AlbumJob, cand core.Candidate, detail string, now time.Time) error {
	m.log().Info(detail, "album_job", job.ID)
	m.recordEvent(ctx, job.ID, core.EventAttemptFailed, detail, now)
	m.recordOutcome(ctx, job, cand.Username, false, now)
	if _, err := m.p.Store.RejectCandidateAndAdvance(ctx, cand.ID, job.ID, "import not confirmed", core.StateImporting, core.StateSelecting, now); err != nil {
		return err
	}
	return nil
}
