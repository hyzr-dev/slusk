package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// MetricsSink receives reconciliation metrics. A nil sink is a no-op, so
// Downloading never depends on the observ package directly. Copied verbatim
// from the legacy engine's MetricsSink (engine/engine.go:14-18); the gauge
// names are kept exactly so existing dashboards/alerts keep working after the
// pipeline rewrite absorbs the reconcile loop.
type MetricsSink interface {
	IncReconcile()
	SetUnknownTransfers(n int)
	SetDownloadsActive(n int)
}

// ReconcileStats summarizes one reconciliation pass. Ported from the legacy
// engine's ReconcileStats (engine/reconciler.go:13-21), with Lost renamed to
// Parked: the retry-budget-exhausted "still in our DB, gone from the backend's
// live list" case now parks the owning job in PARKED for manual operator
// action (issue #158) rather than silently erroring the transfer.
type ReconcileStats struct {
	Adopted   int
	Completed int
	Cancelled int
	Parked    int
	Retried   int
	Stalled   int
	Unknown   int
}

// DownloadingStore is the slice of the store Downloading needs across all three
// of its tick phases. It is declared here (Go style: the consumer owns the
// interface) rather than reusing the legacy engine's JobStore, so the pipeline
// package has no dependency on internal/engine. The concrete *store.Store
// satisfies it implicitly.
type DownloadingStore interface {
	// --- Reconcile phase (port of Reconciler.Reconcile) ---
	TransfersPastDeadline(ctx context.Context, now time.Time) ([]core.Transfer, error)
	ActiveTransfers(ctx context.Context) ([]core.Transfer, error)
	// UpdateTransferProgress is shared by all three phases.
	UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error
	// RetryTransfer is shared by the reconcile and top-up phases.
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	// AttachTransferID is shared by the reconcile and top-up phases. false means
	// the owning job's cancellation barrier won.
	AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (bool, error)
	// ParkJobForCandidate atomically terminalizes an exhausted transfer and parks
	// its DOWNLOADING job, so an operator can manually retry or delete it
	// (issue #158). false means the job already left DOWNLOADING.
	ParkJobForCandidate(ctx context.Context, transferID, candidateID int64, transferState core.TransferState, bytesDone, bytesTotal int64, now time.Time) (bool, error)

	// --- Resolve phase (port of resolveDownloadingJob) ---
	// RunnableJobsInState is used with StateDownloading to pick this tick's
	// batch for both the resolve and top-up phases.
	RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
	// ActiveCandidate returns a DOWNLOADING job's single active candidate.
	ActiveCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error)
	// TransfersForCandidate is shared by the resolve and top-up phases.
	TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)
	// FailCandidateAndAdvance atomically fails a candidate and returns its job to
	// SELECTING in one tx (DOWNLOADING→SELECTING on per-candidate failure), so a
	// job is never left in DOWNLOADING with no ACTIVE candidate.
	FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error)
	// AdvanceJobStateFrom conditionally transitions a job (DOWNLOADING→IMPORTING
	// on success; the candidate stays ACTIVE for the Importing module).
	AdvanceJobStateFrom(ctx context.Context, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error)
	// SucceedCandidateAndAdvance atomically marks the candidate SUCCEEDED and
	// advances the job in one tx. Used only for DOWNLOADING→NOT_IMPORTED
	// (issue #59): unlike the →IMPORTING path above, that destination is
	// terminal, so the candidate has to be terminalized here rather than left
	// ACTIVE for a module that will never pick the job up.
	SucceedCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error)
	// RecordAttemptOutcome writes a candidate's terminal success/fail outcome to
	// the peer reliability tables (best-effort).
	RecordAttemptOutcome(ctx context.Context, artistID int64, username string, success bool, now time.Time) error
	// AddJobEvent appends one row to a job's audit trail.
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error

	// --- Top-up phase (topUpDeps's store half; see selecting.go) ---
	RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, bool, error)
}

// DownloadingParams configures a Downloading.
type DownloadingParams struct {
	Store   DownloadingStore
	Network PeerNetwork  // ListDownloads/Cancel, for the reconcile phase
	Peers   PeerSearcher // Enqueue/Cancel/DeleteDownloadFolder, for resolve + top-up
	Logger  *slog.Logger
	// Metrics receives reconcile stats each tick. nil → metrics are not fed
	// (no-op), so Downloading never panics without an observ sink wired.
	Metrics MetricsSink

	MaxActive          int
	MaxTransferRetries int
	StallTimeout       time.Duration
	MaxInflightPerPeer int
	TransferDeadline   time.Duration
	Interval           time.Duration
}

// TransfersForCandidate, RecordEnqueueIntent, RetryTransfer,
// UpdateTransferProgress, AttachTransferID and Enqueue forward to Store and
// Peers so DownloadingParams itself satisfies topUpDeps (see selecting.go's
// topUpDeps doc comment) - the same forwarding-method pattern SelectingParams
// uses, keeping the two modules' wiring independent rather than sharing one
// deps struct.
func (p DownloadingParams) TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error) {
	return p.Store.TransfersForCandidate(ctx, candidateID)
}

func (p DownloadingParams) RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, bool, error) {
	return p.Store.RecordEnqueueIntent(ctx, candidateID, username, filename, deadline, now)
}

func (p DownloadingParams) RetryTransfer(ctx context.Context, transferID int64, now time.Time) error {
	return p.Store.RetryTransfer(ctx, transferID, now)
}

func (p DownloadingParams) UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error {
	return p.Store.UpdateTransferProgress(ctx, transferID, state, bytesDone, bytesTotal, now)
}

func (p DownloadingParams) AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (bool, error) {
	return p.Store.AttachTransferID(ctx, transferID, remoteID, now)
}

func (p DownloadingParams) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	return p.Peers.Enqueue(ctx, username, filename, size)
}

func (p DownloadingParams) Cancel(ctx context.Context, username, id string) error {
	return p.Peers.Cancel(ctx, username, id)
}

// Downloading owns every DOWNLOADING-state job. Each tick runs three phases in
// a fixed order that is load-bearing:
//
//  1. Reconcile — compare our persisted transfers against slskd's live list and
//     settle the differences (adopt, complete, retry-lost, cancel-overdue,
//     stall-cancel). Ported from the legacy engine's Reconciler.
//  2. Resolve — for each DOWNLOADING job whose active candidate is fully
//     settled, advance it (all transfers COMPLETED → IMPORTING; any transfer
//     failed, once every sibling is terminal → SELECTING). Ported from the
//     legacy Discoverer.resolveDownloadingJob.
//  3. Top-up — release more of each still-downloading candidate's PENDING files
//     to slskd as earlier ones finish. Ported from topUpDownloads.
//
// Resolve MUST run before top-up (same reason advanceDownloading ran before
// topUpDownloads in the legacy engine): the resolve phase's two-phase fail
// path marks a failed candidate's never-sent PENDING siblings CANCELLED, so
// top-up won't release them into a folder resolve is about to delete.
//
// There is deliberately NO separate startup sweep. In the legacy engine a
// standalone SweepStaleDownloads ran once at boot to drain DOWNLOADING jobs
// whose transfers the reconcile loop had already driven terminal while the
// discovery loop was stalled. Here reconcile (phase 1) followed by resolve
// (phase 2) in the same tick IS that sweep: the very first tick reconciles
// against slskd and then resolves every settled job, bounded by MaxActive
// (which is also the ceiling on how many jobs can be DOWNLOADING at once), so a
// crash-legacy backlog drains on tick one. This is a deliberate spec decision,
// not an oversight: do NOT "fix" it by re-adding a separate sweep pass.
type Downloading struct {
	p DownloadingParams
}

// NewDownloading constructs a Downloading.
func NewDownloading(p DownloadingParams) *Downloading {
	if p.Logger != nil {
		p.Logger = p.Logger.With("module", "downloading")
	}
	return &Downloading{p: p}
}

// Name identifies this module in logs and Health().
func (d *Downloading) Name() string { return "downloading" }

// Interval is how often this module ticks.
func (d *Downloading) Interval() time.Duration { return d.p.Interval }

func (d *Downloading) log() *slog.Logger {
	if d.p.Logger != nil {
		return d.p.Logger
	}
	return slog.Default()
}

// recordEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it is
// logged at warn level and swallowed rather than propagated (same pattern as
// Discovery.recordEvent / Selecting.recordEvent).
func (d *Downloading) recordEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) {
	if err := d.p.Store.AddJobEvent(ctx, jobID, event, detail, now); err != nil {
		d.log().Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
	}
}

// recordOutcome writes a candidate's terminal success/fail outcome to the peer
// reliability tables (see Store.RecordAttemptOutcome), so the history survives
// even a subsequent candidate-cache wipe. Best-effort: a failure to record must
// not block the job's own state transition, so it is logged and swallowed
// rather than propagated. Ported from the legacy Discoverer.recordOutcome
// (engine/discovery.go:473-478).
func (d *Downloading) recordOutcome(ctx context.Context, job core.AlbumJob, username string, success bool, now time.Time) {
	if err := d.p.Store.RecordAttemptOutcome(ctx, job.ArtistID, username, success, now); err != nil {
		d.log().Error("record peer reliability outcome failed",
			"album_job", job.ID, "user", username, "success", success, "err", err)
	}
}

// Tick runs the three phases in order (see the Downloading doc comment for why
// the ordering is load-bearing).
func (d *Downloading) Tick(ctx context.Context, now time.Time) error {
	stats, err := d.reconcile(ctx, now)
	// IncReconcile is bumped on every pass, error or not, exactly as the legacy
	// reconcileOnce did, so the metric counts attempts rather than successes.
	if d.p.Metrics != nil {
		d.p.Metrics.IncReconcile()
	}
	if err != nil {
		// A reconcile failure (e.g. slskd ListDownloads unreachable) aborts the
		// tick: the resolve/top-up phases operate on transfer state this pass was
		// supposed to refresh, so acting on stale state would be wrong. The next
		// tick retries the whole sequence. The runner logs the returned error.
		return err
	}
	if d.p.Metrics != nil {
		d.p.Metrics.SetUnknownTransfers(stats.Unknown)
		d.p.Metrics.SetDownloadsActive(stats.Adopted)
	}
	// Log a heartbeat only when the pass actually changed something, so a quiet
	// tick stays silent but real transfer activity is visible. Ported from the
	// legacy reconcileOnce heartbeat (engine/engine.go:205-210).
	if (stats.Adopted + stats.Completed + stats.Cancelled + stats.Parked + stats.Stalled) > 0 {
		d.log().Info("reconciled transfers",
			"adopted", stats.Adopted, "completed", stats.Completed,
			"cancelled", stats.Cancelled, "parked", stats.Parked,
			"stalled", stats.Stalled, "unknown", stats.Unknown)
	}

	if err := d.resolve(ctx, now); err != nil {
		return err
	}
	return d.topUpDownloads(ctx, now)
}

// removeFromSlskd best-effort purges a terminal transfer's leftover record from
// the peer backend. Call it ONLY after the store has marked the transfer
// terminal, or retried via RetryTransfer (which resets it to PENDING and
// detaches its slskd_id) — either way it is no longer in ActiveTransfers.
// Removing it while the store still lists it active would make the next
// reconcile pass see it gone from the live list and treat it as "lost". A 404 is routine (the backend
// already forgot it, e.g. after a slskd restart); any other failure is logged
// and swallowed, with the backend's own retention/cleanup as the backstop.
func (d *Downloading) removeFromSlskd(ctx context.Context, username, id string) {
	if id == "" {
		return
	}
	if err := d.p.Network.Remove(ctx, username, id); err != nil && !errors.Is(err, core.ErrRemoteNotFound) {
		d.log().Warn("remove slskd transfer failed", "user", username, "slskd_id", id, "err", err)
	}
}

// reconcile performs one pass: adopt live transfers, advance terminal ones,
// mark lost ones, and cancel anything past its deadline. Ported verbatim from
// the legacy engine's Reconciler.Reconcile (engine/reconciler.go:83-212),
// s/r.peers/d.p.Network, s/r.store/d.p.Store, s/r.maxRetries/d.p.MaxTransferRetries,
// s/r.stallTimeout/d.p.StallTimeout.
func (d *Downloading) reconcile(ctx context.Context, now time.Time) (ReconcileStats, error) {
	var stats ReconcileStats

	live, err := d.p.Network.ListDownloads(ctx)
	if err != nil {
		return stats, err
	}
	liveByID := map[string]core.RemoteTransfer{}
	liveByFallback := map[string]core.RemoteTransfer{}
	ourIDs := map[string]bool{}
	for _, t := range live {
		liveByID[t.ID] = t
		liveByFallback[t.Username+"\x00"+t.Filename] = t
	}
	matchLive := func(tr core.Transfer) (core.RemoteTransfer, bool) {
		if tr.SlskdID != "" {
			if lt, ok := liveByID[tr.SlskdID]; ok {
				return lt, true
			}
		}
		lt, ok := liveByFallback[tr.Username+"\x00"+tr.Filename]
		return lt, ok
	}
	// STALLED is a durable local intent: it is written before remote
	// cancellation so a later pass can distinguish our stall cancellation from
	// an unrelated cancellation, even if the retry/error write failed.
	finishStalled := func(tr core.Transfer, lt core.RemoteTransfer) error {
		if tr.Retries < d.p.MaxTransferRetries {
			if err := d.p.Store.RetryTransfer(ctx, tr.ID, now); err != nil {
				return fmt.Errorf("retry stalled transfer: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, lt.ID, err)
			}
		} else {
			if err := d.p.Store.UpdateTransferProgress(ctx, tr.ID, core.TransferErrored, tr.BytesDone, tr.BytesTotal, now); err != nil {
				return fmt.Errorf("mark stalled transfer errored: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, lt.ID, err)
			}
		}
		// Production slskd retains cancellation as "Completed, Cancelled".
		// Purge it only after the local retry/error transition has committed.
		d.removeFromSlskd(ctx, tr.Username, lt.ID)
		stats.Stalled++
		return nil
	}

	// Deadline enforcement runs first; a past-deadline transfer must be cancelled
	// and taking it here keeps the active loop from double-processing the same row.
	handled := map[int64]bool{}
	overdue, err := d.p.Store.TransfersPastDeadline(ctx, now)
	if err != nil {
		return stats, err
	}
	for _, tr := range overdue {
		lt, matched := matchLive(tr)
		effectiveID := tr.SlskdID
		if matched {
			ourIDs[lt.ID] = true
			if effectiveID == "" && lt.ID != "" {
				// Recover the id we lost in a crash so we can actually cancel it.
				effectiveID = lt.ID
				attached, err := d.p.Store.AttachTransferID(ctx, tr.ID, lt.ID, now)
				if err != nil {
					return stats, fmt.Errorf("attach transfer id: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, lt.ID, err)
				}
				if !attached {
					// The cancellation barrier won after this stale reconcile
					// snapshot. Compensate the newly discovered remote identity.
					if err := d.p.Network.Cancel(ctx, tr.Username, lt.ID); err != nil {
						d.log().Warn("compensating reconciled remote cancel failed", "transfer", tr.ID, "user", tr.Username, "remote_id", lt.ID, "err", err)
					}
					handled[tr.ID] = true
					continue
				}
			}
			// A failed local retry/error write may outlive the original deadline.
			// Preserve its already-recorded stall intent instead of reclassifying our
			// terminal remote cancellation as an overdue/user cancellation.
			if tr.State == core.TransferStalled && lt.State == core.TransferCancelled {
				if err := finishStalled(tr, lt); err != nil {
					return stats, err
				}
				handled[tr.ID] = true
				continue
			}
			// Still live in slskd: it MUST be cancelled there before we record it
			// cancelled, otherwise we orphan an in-flight download.
			if err := d.p.Network.Cancel(ctx, tr.Username, effectiveID); err != nil {
				// Leave non-terminal; the next pass retries.
				continue
			}
		}
		if err := d.p.Store.UpdateTransferProgress(ctx, tr.ID, core.TransferCancelled, tr.BytesDone, tr.BytesTotal, now); err != nil {
			return stats, fmt.Errorf("mark transfer cancelled: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, effectiveID, err)
		}
		if matched {
			// It existed in slskd (we just cancelled it there); purge the record.
			d.removeFromSlskd(ctx, tr.Username, effectiveID)
		}
		stats.Cancelled++
		handled[tr.ID] = true
	}

	active, err := d.p.Store.ActiveTransfers(ctx)
	if err != nil {
		return stats, err
	}
	for _, tr := range active {
		if handled[tr.ID] {
			continue
		}
		lt, ok := matchLive(tr)
		if !ok {
			// In our DB, gone from slskd's live list: lost. This is the path a slskd
			// restart takes - it wipes its in-memory transfer list, so every transfer
			// that was in flight looks "lost" even though nothing about the download
			// itself failed. Since the peer is usually still willing to resend, treat
			// this the same as a transient rejection: retry within the shared budget
			// rather than failing the whole attempt outright. Bounded so a transfer
			// that keeps vanishing (rather than recovering after a restart) still
			// errors out instead of retrying forever.
			if tr.Retries < d.p.MaxTransferRetries {
				if err := d.p.Store.RetryTransfer(ctx, tr.ID, now); err != nil {
					return stats, fmt.Errorf("retry lost transfer: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, tr.SlskdID, err)
				}
				stats.Retried++
				continue
			}
			// Budget exhausted: this transfer keeps vanishing rather than
			// recovering, so it is no longer a transient slskd restart. Atomically
			// drive the transfer to ERRORED (so later ticks do not rediscover it)
			// and park the owning job for manual action. The guarded job transition
			// may bounce if another module already moved it, but the ERRORED transfer
			// still commits; any database failure rolls both writes back.
			ok, err := d.p.Store.ParkJobForCandidate(ctx, tr.ID, tr.CandidateID, core.TransferErrored, tr.BytesDone, tr.BytesTotal, now)
			if err != nil {
				return stats, fmt.Errorf("park job for lost transfer: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, tr.SlskdID, err)
			}
			// Only count a parked job when this call actually flipped the job to
			// PARKED. On a race where the job already left DOWNLOADING (e.g.
			// WantedSync cancelled it), the transfer is still correctly marked
			// ERRORED above, but there is no newly parked job to report.
			if ok {
				stats.Parked++
			}
			continue
		}
		ourIDs[lt.ID] = true
		if tr.SlskdID == "" && lt.ID != "" {
			// Recover from a crash between RecordEnqueueIntent and AttachTransferID.
			attached, err := d.p.Store.AttachTransferID(ctx, tr.ID, lt.ID, now)
			if err != nil {
				return stats, fmt.Errorf("attach transfer id: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, lt.ID, err)
			}
			if !attached {
				// The cancellation barrier won after ActiveTransfers returned this
				// stale row. Do not let reconciliation bounce it back to live.
				if err := d.p.Network.Cancel(ctx, tr.Username, lt.ID); err != nil {
					d.log().Warn("compensating reconciled remote cancel failed", "transfer", tr.ID, "user", tr.Username, "remote_id", lt.ID, "err", err)
				}
				continue
			}
		}
		newState := lt.State
		if tr.State == core.TransferStalled && newState == core.TransferCancelled {
			if err := finishStalled(tr, lt); err != nil {
				return stats, err
			}
			continue
		}
		// A transient rejection (e.g. a peer's "Too many megabytes" queue limit)
		// with retries left goes back to PENDING for a later resend rather than
		// failing the whole attempt and discarding a peer that has the album.
		if newState == core.TransferErrored && tr.Retries < d.p.MaxTransferRetries && lt.Retryable {
			if err := d.p.Store.RetryTransfer(ctx, tr.ID, now); err != nil {
				return stats, fmt.Errorf("retry rejected transfer: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, lt.ID, err)
			}
			// The native backend's Enqueue is idempotent (it returns the existing
			// terminal transfer rather than restarting it), so the remote record
			// must be removed here for the re-enqueue on the next top-up pass to
			// actually start fresh. Mirrors finishStalled's purge-after-retry above.
			d.removeFromSlskd(ctx, tr.Username, lt.ID)
			stats.Retried++
			continue
		}
		// A transfer still IN_PROGRESS but making no byte progress for longer than
		// StallTimeout is treated as dead: the peer stopped sending without
		// disconnecting, so it would otherwise live on until its enqueue-relative
		// deadline. Persist STALLED intent, cancel it in slskd, then retry within
		// budget or error it out once the budget is spent, reclaiming the attempt
		// early.
		if newState == core.TransferInProgress && tr.LastProgressAt != nil &&
			now.Sub(*tr.LastProgressAt) > d.p.StallTimeout {
			if tr.State != core.TransferStalled {
				if err := d.p.Store.UpdateTransferProgress(ctx, tr.ID, core.TransferStalled, lt.BytesDone, lt.Size, now); err != nil {
					return stats, fmt.Errorf("mark transfer stalled: transfer %d candidate %d remote %q: %w", tr.ID, tr.CandidateID, lt.ID, err)
				}
			}
			if err := d.p.Network.Cancel(ctx, tr.Username, lt.ID); err != nil {
				// The STALLED intent remains durable; the next pass retries cancel.
				continue
			}
			if err := finishStalled(tr, lt); err != nil {
				return stats, err
			}
			continue
		}
		if err := d.p.Store.UpdateTransferProgress(ctx, tr.ID, newState, lt.BytesDone, lt.Size, now); err != nil {
			return stats, fmt.Errorf("update transfer progress to %s: transfer %d candidate %d remote %q: %w", newState, tr.ID, tr.CandidateID, lt.ID, err)
		}
		if newState == core.TransferCompleted || newState == core.TransferCancelled || newState == core.TransferErrored {
			// Reached a terminal state and the store write above is committed, so
			// slskd's now-stale record can be purged. slskd keeps terminal
			// transfers in its list ("Completed, <Outcome>") forever otherwise.
			d.removeFromSlskd(ctx, lt.Username, lt.ID)
		}
		switch newState {
		case core.TransferCompleted:
			stats.Completed++
		default:
			stats.Adopted++
		}
	}

	for _, t := range live {
		if !ourIDs[t.ID] {
			stats.Unknown++
		}
	}
	return stats, nil
}

// resolve advances every runnable DOWNLOADING job one step, bounded by
// MaxActive. Ported from the legacy Discoverer.advanceDownloading
// (engine/discovery.go:509-520): MaxActive, not Batch, because DOWNLOADING can
// hold up to MaxActive jobs at once, all of which need a chance to resolve
// every tick.
func (d *Downloading) resolve(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.RunnableJobsInState(ctx, core.StateDownloading, now, d.p.MaxActive)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if _, err := d.resolveDownloadingJob(ctx, job, now); err != nil {
			return err
		}
	}
	return nil
}

// resolveDownloadingJob advances one DOWNLOADING job to IMPORTING (all
// transfers completed) or SELECTING (any transfer failed, once every sibling
// has reached a terminal state), or leaves it untouched if genuinely still in
// flight. Returns whether the job's state changed.
//
// The fail path is two-phase: cancel first, clean up once everything is quiet.
// When a candidate fails while some of its siblings are still QUEUED/IN_PROGRESS/
// STALLED in slskd, cleaning up (deleting the candidate's download folder) would
// race those live downloads - slskd keeps writing their bytes back into the
// folder we just deleted, re-creating exactly the cross-peer collision
// cleanupFolder exists to prevent. So we first cancel the still-active siblings
// in slskd (and mark never-sent PENDING siblings CANCELLED directly, since there
// is nothing in slskd to cancel), then wait: cleanup, FailCandidate and the
// state transition are deferred to a later call once every transfer is terminal.
//
// Ported from the legacy Discoverer.resolveDownloadingJob
// (engine/discovery.go:568-669). The pipeline differences from the legacy code:
// the "active attempt" is now ActiveCandidate; success advances straight to
// IMPORTING (VERIFYING no longer exists); and the fail path does NOT set a
// cooldown or bump a retries counter - a per-candidate failure is free, so the
// next cached candidate is tried immediately on the next SELECTING tick.
func (d *Downloading) resolveDownloadingJob(ctx context.Context, job core.AlbumJob, now time.Time) (bool, error) {
	cand, found, err := d.p.Store.ActiveCandidate(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	transfers, err := d.p.Store.TransfersForCandidate(ctx, cand.ID)
	if err != nil {
		return false, err
	}
	allDone, anyFailed := len(transfers) > 0, false
	var activeSiblings, pendingSiblings []core.Transfer
	for _, t := range transfers {
		switch t.State {
		case core.TransferCompleted:
		case core.TransferErrored, core.TransferCancelled:
			anyFailed = true
			allDone = false
		case core.TransferPending:
			// Never sent to slskd; nothing to cancel there, but still not terminal.
			pendingSiblings = append(pendingSiblings, t)
			allDone = false
		default: // QUEUED, IN_PROGRESS, STALLED: still live in slskd.
			activeSiblings = append(activeSiblings, t)
			allDone = false
		}
	}
	switch {
	case anyFailed:
		// A candidate failed, but other untried candidates usually remain cached,
		// so the next SELECTING tick tries one immediately - no cooldown here.
		//
		// Two-phase fail: never-sent PENDING siblings are cancelled straight in
		// the DB (nothing exists in slskd to cancel, and without this the candidate
		// would never reach "all terminal"); still-active siblings are cancelled
		// in slskd. While any sibling is still live we defer cleanup/FailCandidate/
		// state transition to a later call - deleting the folder now would race those
		// downloads. The caller re-runs every tick, so the job converges to
		// "all terminal" within a few ticks.
		for _, t := range pendingSiblings {
			if err := d.p.Store.UpdateTransferProgress(ctx, t.ID, core.TransferCancelled, t.BytesDone, t.BytesTotal, now); err != nil {
				d.log().Error("cancel pending sibling failed", "transfer", t.ID, "err", err)
			}
		}
		if len(activeSiblings) > 0 {
			d.log().Info("candidate failed, cancelling active siblings before cleanup",
				"album_job", job.ID, "candidate", cand.ID, "active", len(activeSiblings))
			for _, t := range activeSiblings {
				if t.SlskdID == "" {
					// No slskd id yet (enqueue never returned one); the reconciler's
					// fallback matching will terminate it. Leave it for a later tick.
					continue
				}
				err := d.p.Peers.Cancel(ctx, t.Username, t.SlskdID)
				switch {
				case err == nil:
					// Cancelled in slskd; the reconciler's next pass will observe it
					// gone from slskd's live list and mark it terminal on our side.
				case errors.Is(err, core.ErrRemoteNotFound):
					// slskd already forgot this transfer (e.g. it restarted, or the
					// transfer silently dropped out of its live list): there is
					// nothing left to cancel, so treat it as already-terminal here
					// rather than retrying a cancel that would 404 forever and wedge
					// this job in DOWNLOADING permanently.
					if uerr := d.p.Store.UpdateTransferProgress(ctx, t.ID, core.TransferCancelled, t.BytesDone, t.BytesTotal, now); uerr != nil {
						d.log().Error("mark vanished sibling cancelled failed", "transfer", t.ID, "err", uerr)
					}
				default:
					// Leave it active; the next tick retries the cancel.
					d.log().Error("cancel active sibling failed", "album_job", job.ID, "transfer", t.ID, "err", err)
				}
			}
			return false, nil
		}
		// Every transfer is terminal: safe to clean up and fail the candidate.
		failedDetail := fmt.Sprintf("candidate %s download failed, trying next candidate", cand.Username)
		d.log().Info(failedDetail, "album_job", job.ID, "candidate", cand.ID)
		d.recordEvent(ctx, job.ID, core.EventAttemptFailed, failedDetail, now)
		names := make([]string, 0, len(transfers))
		for _, t := range transfers {
			names = append(names, t.Filename)
		}
		cleanupFolder(ctx, d.p.Peers, d.log(), job.ID, names)
		d.recordOutcome(ctx, job, cand.Username, false, now)
		// Fail the candidate and return the job to SELECTING atomically: the two
		// writes commit together or not at all, so the job can never be stranded
		// in DOWNLOADING with no ACTIVE candidate (which would permanently consume
		// a MaxActive slot). Per-candidate failure is free: no cooldown, no retries
		// bump - the next cached candidate is tried immediately on the next tick.
		if _, err := d.p.Store.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "transfer failed", core.StateDownloading, core.StateSelecting, now); err != nil {
			return false, err
		}
		return true, nil
	case allDone && job.Source == core.SourceManual && job.AlbumMBID == "":
		// A manual job the user never identified against a MusicBrainz
		// release group has nothing for Importing to import into (issue
		// #59): its LidarrAlbumID would stay 0 forever, and
		// Music.AlbumStatus(0) errors on every tick. Route straight to the
		// terminal NOT_IMPORTED instead - the files are the deliverable, so
		// no cleanup runs here (contrast the anyFailed branch above, which
		// does clean up a genuinely failed download).
		//
		// The candidate is SUCCEEDED and the peer credited: it delivered every
		// file asked of it, and NOT_IMPORTED is terminal, so leaving the
		// candidate ACTIVE would strand it in ActiveCandidate and make the job
		// detail view show an attempt still in progress on a finished job.
		notImportedDetail := "download complete, no album identified - leaving files in place"
		d.log().Info(notImportedDetail, "album_job", job.ID)
		d.recordEvent(ctx, job.ID, core.EventNotImported, notImportedDetail, now)
		d.recordOutcome(ctx, job, cand.Username, true, now)
		if _, err := d.p.Store.SucceedCandidateAndAdvance(ctx, cand.ID, job.ID, core.StateDownloading, core.StateNotImported, now); err != nil {
			d.log().Error("advance to not imported failed", "album_job", job.ID, "err", err)
		}
		return true, nil
	case allDone:
		d.log().Info("download complete, importing", "album_job", job.ID)
		if _, err := d.p.Store.AdvanceJobStateFrom(ctx, job.ID, core.StateDownloading, core.StateImporting, now); err != nil {
			d.log().Error("advance to importing failed", "album_job", job.ID, "err", err)
		}
		return true, nil
	}
	return false, nil
}

// topUpDownloads promotes more PENDING files for every DOWNLOADING job whose
// in-flight count has dropped below MaxInflightPerPeer, so a throttled candidate
// keeps making progress across ticks as its earlier files complete. Ported from
// the legacy Discoverer.topUpDownloads (engine/discovery.go:427-451), with the
// active attempt now sourced via ActiveCandidate and the shared topUpCandidate
// (task 8) doing the actual enqueueing. Bounded by MaxActive, not Batch - every
// DOWNLOADING job needs a chance to resend its pending files each tick.
func (d *Downloading) topUpDownloads(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.RunnableJobsInState(ctx, core.StateDownloading, now, d.p.MaxActive)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		cand, found, err := d.p.Store.ActiveCandidate(ctx, job.ID)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		sent, err := topUpCandidate(ctx, d.p, cand.ID, now, d.p.MaxInflightPerPeer, d.p.MaxTransferRetries, d.p.TransferDeadline, d.log())
		if err != nil {
			d.log().Error("top up downloads failed", "album_job", job.ID, "err", err)
		}
		if sent > 0 {
			d.log().Info("released deferred downloads",
				"album_job", job.ID, "user", cand.Username, "sent", sent)
		}
	}
	return nil
}
