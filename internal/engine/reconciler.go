package engine

import (
	"context"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// ReconcileStats summarizes one reconciliation pass.
type ReconcileStats struct {
	Adopted   int
	Completed int
	Cancelled int
	Lost      int
	Retried   int
	Stalled   int
	Unknown   int
}

// Reconciler compares slskdarr's persisted transfers against slskd's live list
// and reconciles the differences. It runs identically at startup and on a timer.
type Reconciler struct {
	peers        PeerNetwork
	store        JobStore
	maxRetries   int
	stallTimeout time.Duration
}

// NewReconciler constructs a Reconciler. maxRetries bounds how many times a
// transfer rejected for a transient reason (e.g. a peer's queued-megabyte
// limit) is re-queued before it is finally errored. stallTimeout is how long an
// IN_PROGRESS transfer may go without byte progress before it is cancelled and
// retried, so a dead download is reclaimed early rather than waiting out its
// (enqueue-relative) deadline.
func NewReconciler(peers PeerNetwork, store JobStore, maxRetries int, stallTimeout time.Duration) *Reconciler {
	return &Reconciler{peers: peers, store: store, maxRetries: maxRetries, stallTimeout: stallTimeout}
}

// mapSlskdState translates a slskd transfer state string to our TransferState.
func mapSlskdState(s string) core.TransferState {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "completed"):
		// slskd reports every terminal outcome as "Completed, <Outcome>": Succeeded,
		// Cancelled, TimedOut, Errored, Rejected, or Aborted. Only Succeeded is a win;
		// Cancelled is our/its cancellation; everything else (timed out, rejected,
		// aborted, errored) is a terminal failure.
		switch {
		case strings.Contains(l, "succeeded"):
			return core.TransferCompleted
		case strings.Contains(l, "cancelled"), strings.Contains(l, "canceled"):
			return core.TransferCancelled
		default:
			return core.TransferErrored
		}
	case strings.Contains(l, "inprogress"):
		return core.TransferInProgress
	default:
		return core.TransferQueued
	}
}

// isRetryable reports whether a slskd failure reason is transient (worth
// re-queueing) rather than permanent. Permanent reasons - the peer will never
// serve this file - are matched by a denylist; everything else, including a
// peer's "Too many megabytes" queue-limit rejection, is treated as retryable.
// An empty reason is retryable: the bounded retry count keeps it from looping.
func isRetryable(exception string) bool {
	e := strings.ToLower(exception)
	for _, permanent := range []string{"file not shared", "not shared", "banned"} {
		if strings.Contains(e, permanent) {
			return false
		}
	}
	return true
}

// Reconcile performs one pass: adopt live transfers, advance terminal ones,
// mark lost ones, and cancel anything past its deadline.
func (r *Reconciler) Reconcile(ctx context.Context, now time.Time) (ReconcileStats, error) {
	var stats ReconcileStats

	live, err := r.peers.ListDownloads(ctx)
	if err != nil {
		return stats, err
	}
	liveByID := map[string]slskd.Transfer{}
	liveByFallback := map[string]slskd.Transfer{}
	ourIDs := map[string]bool{}
	for _, t := range live {
		liveByID[t.ID] = t
		liveByFallback[t.Username+"\x00"+t.Filename] = t
	}
	matchLive := func(tr core.Transfer) (slskd.Transfer, bool) {
		if tr.SlskdID != "" {
			if lt, ok := liveByID[tr.SlskdID]; ok {
				return lt, true
			}
		}
		lt, ok := liveByFallback[tr.Username+"\x00"+tr.Filename]
		return lt, ok
	}

	// Deadline enforcement runs first; a past-deadline transfer must be cancelled
	// and taking it here keeps the active loop from double-processing the same row.
	handled := map[int64]bool{}
	overdue, err := r.store.TransfersPastDeadline(ctx, now)
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
				_ = r.store.AttachTransferID(ctx, tr.ID, lt.ID, now)
			}
			// Still live in slskd: it MUST be cancelled there before we record it
			// cancelled, otherwise we orphan an in-flight download.
			if err := r.peers.Cancel(ctx, tr.Username, effectiveID); err != nil {
				// Leave non-terminal; the next pass retries.
				continue
			}
		}
		_ = r.store.UpdateTransferProgress(ctx, tr.ID, core.TransferCancelled, tr.BytesDone, tr.BytesTotal, now)
		stats.Cancelled++
		handled[tr.ID] = true
	}

	active, err := r.store.ActiveTransfers(ctx)
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
			if tr.Retries < r.maxRetries {
				_ = r.store.RetryTransfer(ctx, tr.ID, now)
				stats.Retried++
				continue
			}
			_ = r.store.UpdateTransferProgress(ctx, tr.ID, core.TransferErrored, tr.BytesDone, tr.BytesTotal, now)
			stats.Lost++
			continue
		}
		ourIDs[lt.ID] = true
		if tr.SlskdID == "" && lt.ID != "" {
			// Recover from a crash between RecordEnqueueIntent and AttachTransferID.
			_ = r.store.AttachTransferID(ctx, tr.ID, lt.ID, now)
		}
		newState := mapSlskdState(lt.State)
		// A transient rejection (e.g. a peer's "Too many megabytes" queue limit)
		// with retries left goes back to PENDING for a later resend rather than
		// failing the whole attempt and discarding a peer that has the album.
		if newState == core.TransferErrored && tr.Retries < r.maxRetries && isRetryable(lt.Exception) {
			_ = r.store.RetryTransfer(ctx, tr.ID, now)
			stats.Retried++
			continue
		}
		// A transfer still IN_PROGRESS but making no byte progress for longer than
		// stallTimeout is treated as dead: the peer stopped sending without
		// disconnecting, so it would otherwise live on until its enqueue-relative
		// deadline. Cancel it in slskd first (same must-cancel-before-record rule as
		// the deadline path above), then retry within budget or error it out once the
		// budget is spent, reclaiming the attempt early.
		if newState == core.TransferInProgress && tr.LastProgressAt != nil &&
			now.Sub(*tr.LastProgressAt) > r.stallTimeout {
			if err := r.peers.Cancel(ctx, tr.Username, lt.ID); err != nil {
				// Leave it non-terminal; the next pass retries the cancel.
				continue
			}
			if tr.Retries < r.maxRetries {
				_ = r.store.RetryTransfer(ctx, tr.ID, now)
			} else {
				_ = r.store.UpdateTransferProgress(ctx, tr.ID, core.TransferErrored, tr.BytesDone, tr.BytesTotal, now)
			}
			stats.Stalled++
			continue
		}
		_ = r.store.UpdateTransferProgress(ctx, tr.ID, newState, lt.BytesTransferred, lt.Size, now)
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
