package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// SelectingStore is the slice of the store Selecting needs. It embeds
// BackoffStore since exhausting a job's candidate cache goes through the same
// failOrBackoff path as every other module's search-cycle failure.
type SelectingStore interface {
	BackoffStore
	// RunnableJobsInState is used with StateSelecting to pick this tick's
	// batch of jobs (see RunnableJobsInState's doc comment for ordering).
	RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
	// NextNewCandidate returns a job's best untried cached candidate.
	NextNewCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error)
	// ActivateCandidate atomically re-checks the MaxActive cap and, if room
	// remains, activates the candidate and advances the job to DOWNLOADING.
	ActivateCandidate(ctx context.Context, candidateID, jobID int64, maxActive int, now time.Time) (bool, error)
	// RecordPendingTransfer write-aheads one of a newly-activated candidate's
	// files as PENDING, before any of them are handed to slskd.
	RecordPendingTransfer(ctx context.Context, candidateID int64, username, filename string, size int64, now time.Time) error

	// The remaining methods are topUpDeps's store half (see topUpCandidate):
	// declared again here, rather than embedding topUpDeps directly, so this
	// interface's doc comment stays the single place documenting what
	// Selecting needs from the store.
	TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)
	RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, error)
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
}

// SelectingParams configures a Selecting.
type SelectingParams struct {
	Store SelectingStore
	Peers PeerSearcher

	MaxActive  int
	MaxRetries int

	BackoffBase time.Duration
	BackoffCap  time.Duration

	// CandidateTTL bounds how long a cached NEW candidate may sit unactivated
	// before Selecting discards the whole cache and sends the job back to
	// WANTED for a fresh search: a search result this old (peer gone offline,
	// shared files renamed/removed) is no longer trustworthy enough to enqueue
	// blind.
	CandidateTTL time.Duration

	MaxInflightPerPeer int
	MaxTransferRetries int
	TransferDeadline   time.Duration

	Interval time.Duration

	Logger *slog.Logger
}

// TransfersForCandidate, RecordEnqueueIntent, RetryTransfer,
// UpdateTransferProgress, AttachTransferID and Enqueue forward to Store and
// Peers so SelectingParams itself satisfies topUpDeps (see selecting.go's
// topUpDeps doc comment) - Downloading (task 9) does the same on its own
// params struct, keeping the two modules' wiring independent rather than
// sharing one deps struct.
func (p SelectingParams) TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error) {
	return p.Store.TransfersForCandidate(ctx, candidateID)
}

func (p SelectingParams) RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, error) {
	return p.Store.RecordEnqueueIntent(ctx, candidateID, username, filename, deadline, now)
}

func (p SelectingParams) RetryTransfer(ctx context.Context, transferID int64, now time.Time) error {
	return p.Store.RetryTransfer(ctx, transferID, now)
}

func (p SelectingParams) UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error {
	return p.Store.UpdateTransferProgress(ctx, transferID, state, bytesDone, bytesTotal, now)
}

func (p SelectingParams) AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error {
	return p.Store.AttachTransferID(ctx, transferID, slskdID, now)
}

func (p SelectingParams) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	return p.Peers.Enqueue(ctx, username, filename, size)
}

// Selecting activates one cached candidate per runnable SELECTING job per
// tick (bounded by MaxActive), or backs the job off when its candidate cache
// is exhausted or stale. It ports the back half of the legacy engine's
// startJob (candidate selection/enqueue; the front half - searching and
// caching - is Discovery's).
type Selecting struct {
	p SelectingParams
}

// NewSelecting constructs a Selecting.
func NewSelecting(p SelectingParams) *Selecting { return &Selecting{p: p} }

// Name identifies this module in logs and Health().
func (s *Selecting) Name() string { return "selecting" }

// Interval is how often this module ticks.
func (s *Selecting) Interval() time.Duration { return s.p.Interval }

func (s *Selecting) log() *slog.Logger {
	if s.p.Logger != nil {
		return s.p.Logger
	}
	return slog.Default()
}

// recordEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it is
// logged at warn level and swallowed rather than propagated (same pattern as
// Discovery.recordEvent).
func (s *Selecting) recordEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) {
	if err := s.p.Store.AddJobEvent(ctx, jobID, event, detail, now); err != nil {
		s.log().Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
	}
}

// Tick processes every runnable SELECTING job, up to MaxActive of them (the
// MaxActive cap on concurrently DOWNLOADING+IMPORTING jobs means at most
// MaxActive activations could ever succeed in one tick anyway, so a bigger
// batch would only mean wasted NextNewCandidate/ActivateCandidate round trips
// once the cap fills - see selectJob's early-stop below).
func (s *Selecting) Tick(ctx context.Context, now time.Time) error {
	jobs, err := s.p.Store.RunnableJobsInState(ctx, core.StateSelecting, now, s.p.MaxActive)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		proceed, err := s.selectJob(ctx, job, now)
		if err != nil {
			return err
		}
		if !proceed {
			break
		}
	}
	return nil
}

// selectJob tries to activate one job's best cached candidate. The returned
// bool tells Tick whether to keep working the rest of this tick's batch:
// false only when ActivateCandidate reports the MaxActive cap is full (or the
// job left SELECTING underneath us), since every later job in the batch would
// hit the same cap check and fail identically - stopping early saves the
// round trips. Every other outcome (exhaustion, TTL expiry) returns true so
// the batch keeps going.
func (s *Selecting) selectJob(ctx context.Context, job core.AlbumJob, now time.Time) (bool, error) {
	cand, ok, err := s.p.Store.NextNewCandidate(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		// No NEW candidate left to try: every cached candidate from the last
		// search has already been activated and failed. This is a search-cycle
		// failure like Discovery's empty-results case, so it goes through the
		// same failOrBackoff path, but with resetToWanted=true: ResetJobToWanted
		// wipes the (now-useless) candidate cache and its transfers and sends
		// the job back to WANTED for a fresh search, rather than leaving it
		// stuck in SELECTING with nothing left to try.
		detail := "candidates exhausted, re-searching"
		s.log().Info(detail, "album_job", job.ID)
		if err := failOrBackoff(ctx, s.p.Store, s.log(), job, s.p.MaxRetries, s.p.BackoffBase, s.p.BackoffCap, true, now); err != nil {
			return false, err
		}
		s.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
		return true, nil
	}

	if now.Sub(cand.CreatedAt) > s.p.CandidateTTL {
		// The cached candidate is too old to trust: the peer may have gone
		// offline or renamed/removed the shared files since the search ran.
		// Discard the whole cache and re-search from scratch, same as
		// exhaustion, but retries is left UNCHANGED - staleness is not a search
		// failure, and penalizing the retry budget for a job that simply sat in
		// SELECTING too long (behind other jobs at the MaxActive cap) would be
		// wrong.
		detail := "candidate cache expired, re-searching"
		s.log().Info(detail, "album_job", job.ID, "candidate", cand.ID)
		if err := s.p.Store.ResetJobToWanted(ctx, job.ID, core.StateSelecting, job.Retries, nil, now); err != nil {
			return false, err
		}
		s.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
		return true, nil
	}

	activated, err := s.p.Store.ActivateCandidate(ctx, cand.ID, job.ID, s.p.MaxActive, now)
	if err != nil {
		return false, err
	}
	if !activated {
		// Either the MaxActive cap is already full, or the job left SELECTING
		// underneath us (e.g. cancelled). Either way there is nothing to do
		// now; the next tick re-evaluates from RunnableJobsInState.
		s.log().Info("candidate activation skipped, cap full or job left selecting",
			"album_job", job.ID, "candidate", cand.ID)
		return false, nil
	}

	// Write-ahead every file as PENDING (survives a restart) before handing
	// any of them to slskd - ported from legacy startJob's back half
	// (engine/discovery.go:328-351).
	for _, f := range cand.Files {
		if err := s.p.Store.RecordPendingTransfer(ctx, cand.ID, cand.Username, f.Filename, f.Size, now); err != nil {
			return false, err
		}
	}

	sent, err := topUpCandidate(ctx, s.p, cand.ID, now, s.p.MaxInflightPerPeer, s.p.MaxTransferRetries, s.p.TransferDeadline, s.log())
	if err != nil {
		return false, err
	}

	selectedDetail := fmt.Sprintf("enqueued candidate %s, downloading (%d files, %d sent, %d deferred)",
		cand.Username, len(cand.Files), sent, len(cand.Files)-sent)
	s.log().Info(selectedDetail, "album_job", job.ID, "user", cand.Username,
		"files", len(cand.Files), "sent", sent, "deferred", len(cand.Files)-sent)
	s.recordEvent(ctx, job.ID, core.EventCandidateSelected, selectedDetail, now)
	return true, nil
}

// topUpDeps is the store+peer behavior topUpCandidate needs: exactly enough
// to hand a candidate's PENDING files to slskd and record the outcome. Kept
// as a small free-standing interface rather than a shared struct so
// Selecting and Downloading (task 9) can each own their store/peer wiring
// independently - both modules' param structs (SelectingParams, and
// Downloading's equivalent) implement it by forwarding to their own Store and
// Peers fields (see the forwarding methods above SelectingParams).
type topUpDeps interface {
	TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)
	RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, error)
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
	Enqueue(ctx context.Context, username, filename string, size int64) (string, error)
}

// topUpCandidate hands a candidate's PENDING files to slskd until
// maxInflightPerPeer transfers are in flight, sending more only as earlier
// ones finish. Keeping the queued count bounded stops the peer's per-user
// queued-megabyte limit from rejecting a burst. PENDING files carry their
// size in bytes_total (set at RecordPendingTransfer time) so they can be
// enqueued here without the original search result in hand. Files are sent
// in filename order for deterministic progress.
//
// maxInflightPerPeer, maxTransferRetries, transferDeadline and log are passed
// explicitly rather than folded into topUpDeps: they are plain configuration,
// not behavior, so there is nothing to gain from routing them through an
// interface method.
//
// Ported from the legacy engine's topUpAttempt (engine/discovery.go:370-418),
// s/attempt/candidate/. Selecting calls this once, right after activating a
// candidate; Downloading (task 9) calls it again on every tick to keep
// topping up a still-downloading candidate as earlier files finish.
func topUpCandidate(ctx context.Context, d topUpDeps, candidateID int64, now time.Time, maxInflightPerPeer, maxTransferRetries int, transferDeadline time.Duration, log *slog.Logger) (int, error) {
	transfers, err := d.TransfersForCandidate(ctx, candidateID)
	if err != nil {
		return 0, err
	}
	inflight := 0
	var pending []core.Transfer
	for _, tr := range transfers {
		switch tr.State {
		case core.TransferQueued, core.TransferInProgress, core.TransferStalled:
			inflight++
		case core.TransferPending:
			pending = append(pending, tr)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Filename < pending[j].Filename })

	deadline := now.Add(transferDeadline)
	sent := 0
	for _, p := range pending {
		if inflight >= maxInflightPerPeer {
			break
		}
		// Promote PENDING -> QUEUED with a real deadline, then hand it to slskd.
		tid, err := d.RecordEnqueueIntent(ctx, candidateID, p.Username, p.Filename, deadline, now)
		if err != nil {
			return sent, err
		}
		slskdID, err := d.Enqueue(ctx, p.Username, p.Filename, p.BytesTotal)
		if err != nil {
			if p.Retries < maxTransferRetries {
				log.Info("enqueue failed, returning to pending", "user", p.Username, "file", p.Filename, "retries", p.Retries, "err", err)
				if uerr := d.RetryTransfer(ctx, tid, now); uerr != nil {
					log.Error("retry transfer failed", "transfer", tid, "err", uerr)
				}
			} else {
				log.Error("enqueue failed", "user", p.Username, "file", p.Filename, "err", err)
				if uerr := d.UpdateTransferProgress(ctx, tid, core.TransferErrored, 0, 0, now); uerr != nil {
					log.Error("mark transfer errored failed", "transfer", tid, "err", uerr)
				}
			}
			continue
		}
		_ = d.AttachTransferID(ctx, tid, slskdID, now)
		inflight++
		sent++
	}
	return sent, nil
}
