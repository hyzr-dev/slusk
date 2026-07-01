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
	Unknown   int
}

// Reconciler compares slskdarr's persisted transfers against slskd's live list
// and reconciles the differences. It runs identically at startup and on a timer.
type Reconciler struct {
	peers PeerNetwork
	store JobStore
}

// NewReconciler constructs a Reconciler.
func NewReconciler(peers PeerNetwork, store JobStore) *Reconciler {
	return &Reconciler{peers: peers, store: store}
}

// mapSlskdState translates a slskd transfer state string to our TransferState.
func mapSlskdState(s string) core.TransferState {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "completed") && strings.Contains(l, "succeeded"):
		return core.TransferCompleted
	case strings.Contains(l, "errored"), strings.Contains(l, "failed"):
		return core.TransferErrored
	case strings.Contains(l, "cancelled"), strings.Contains(l, "canceled"):
		return core.TransferCancelled
	case strings.Contains(l, "inprogress"):
		return core.TransferInProgress
	default:
		return core.TransferQueued
	}
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
			// In our DB, gone from slskd: lost.
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
