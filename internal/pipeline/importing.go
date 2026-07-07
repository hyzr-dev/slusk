package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
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
	// RecordAttemptOutcome writes a candidate's terminal success/fail outcome to
	// the peer reliability tables (best-effort).
	RecordAttemptOutcome(ctx context.Context, artistID int64, username string, success bool, now time.Time) error
	// AddJobEvent appends one row to a job's audit trail.
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
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
func NewImporting(p ImportingParams) *Importing { return &Importing{p: p} }

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
func (m *Importing) failCandidate(ctx context.Context, job core.AlbumJob, cand core.Candidate, names []string, reason string, now time.Time) error {
	cleanupFolder(ctx, m.p.Peers, m.log(), job.ID, names)
	m.recordOutcome(ctx, job, cand.Username, false, now)
	if _, err := m.p.Store.FailCandidateAndAdvance(ctx, cand.ID, job.ID, reason, core.StateImporting, core.StateSelecting, now); err != nil {
		return err
	}
	return nil
}

// verify is the verify-phase gate (ImportSubmittedAt is NULL): it asks Lidarr
// what it would import from the album folder and decides whether to import at
// all. A candidate with any rejection fails outright (SELECTING, next
// candidate tried immediately - no cooldown). A clean candidate that cannot
// cover the whole release is also failed, rather than importing a partial
// album. A clean, complete candidate is imported and ImportSubmittedAt is set
// so the next tick's confirm phase can confirm Lidarr accepted it.
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
	folder := AlbumFolder(m.p.CompleteDir, names)
	items, err := m.p.Music.ManualImportCandidates(ctx, folder)
	if err != nil {
		m.log().Error("manual import candidates failed", "album_job", job.ID, "folder", folder, "err", err)
		return m.escalateIfStuck(ctx, job, cand, names, "import candidates failed", now)
	}
	if len(items) == 0 {
		// Empty folder on a job whose transfers all completed means the files
		// were already imported (e.g. a crash between a prior successful
		// ExecuteManualImport and this state write). Treat it as done so verify
		// is idempotent across restarts.
		emptyDetail := fmt.Sprintf("empty folder treated as already imported (folder %s)", folder)
		m.log().Info(emptyDetail, "album_job", job.ID, "folder", folder)
		m.recordEvent(ctx, job.ID, core.EventAttemptSucceeded, emptyDetail, now)
		m.recordOutcome(ctx, job, cand.Username, true, now)
		if _, err := m.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateImporting, core.StateDone, now); err != nil {
			return err
		}
		return nil
	}
	var importable []lidarr.ManualImportItem
	var rejections []string
	for _, it := range items {
		if it.Importable {
			importable = append(importable, it)
		} else {
			rejections = append(rejections, it.Rejections...)
		}
	}
	if len(rejections) > 0 || len(importable) == 0 {
		// Rejected like a failed download: other candidates usually remain, so
		// the next SELECTING tick tries one immediately - no cooldown.
		rejectedDetail := fmt.Sprintf("import rejected (folder %s): %s", folder, strings.Join(rejections, "; "))
		m.log().Info(rejectedDetail, "album_job", job.ID, "folder", folder, "reasons", rejections)
		m.recordEvent(ctx, job.ID, core.EventImportRejected, rejectedDetail, now)
		return m.failCandidate(ctx, job, cand, names, "import rejected", now)
	}
	_, total, err := m.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
	if err != nil {
		m.log().Error("album status failed", "album_job", job.ID, "err", err)
		return m.escalateIfStuck(ctx, job, cand, names, "album status check failed", now)
	}
	if coverage(importable) < total {
		// A source that can't complete the release is rejected outright rather
		// than partially imported, to keep the library free of half albums.
		// Other candidates usually remain, so use the no-cooldown fail path.
		incompleteDetail := fmt.Sprintf("incomplete download, rejecting (folder %s, covered %d/%d)", folder, coverage(importable), total)
		m.log().Info(incompleteDetail, "album_job", job.ID, "folder", folder,
			"covered", coverage(importable), "total", total)
		m.recordEvent(ctx, job.ID, core.EventImportRejected, incompleteDetail, now)
		return m.failCandidate(ctx, job, cand, names, "incomplete download", now)
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
func (m *Importing) escalateIfStuck(ctx context.Context, job core.AlbumJob, cand core.Candidate, filenames []string, reason string, now time.Time) error {
	if now.Sub(job.UpdatedAt) <= m.p.StuckAfter {
		return nil
	}
	stuckDetail := fmt.Sprintf("importing stuck past timeout (%s)", reason)
	m.log().Info(stuckDetail, "album_job", job.ID, "reason", reason)
	m.recordEvent(ctx, job.ID, core.EventAttemptFailed, stuckDetail, now)
	return m.failCandidate(ctx, job, cand, filenames, reason, now)
}

// coverage counts the distinct Lidarr track IDs covered by importable, used to
// judge whether a candidate can complete a release's full track count. Ported
// verbatim from the legacy engine (engine/discovery.go:829-837).
func coverage(importable []lidarr.ManualImportItem) int {
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
	if present >= total {
		confirmedDetail := fmt.Sprintf("import confirmed, completed (%d/%d present)", present, total)
		m.log().Info(confirmedDetail, "album_job", job.ID)
		m.recordEvent(ctx, job.ID, core.EventAttemptSucceeded, confirmedDetail, now)
		m.recordOutcome(ctx, job, cand.Username, true, now)
		if _, err := m.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateImporting, core.StateDone, now); err != nil {
			return err
		}
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
	if _, err := m.p.Store.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "import not confirmed", core.StateImporting, core.StateSelecting, now); err != nil {
		return err
	}
	return nil
}
