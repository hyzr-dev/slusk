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

	active, err := r.store.ActiveTransfers(ctx)
	if err != nil {
		return stats, err
	}
	for _, tr := range active {
		lt, ok := liveByID[tr.SlskdID]
		if !ok && tr.SlskdID == "" {
			lt, ok = liveByFallback[tr.Username+"\x00"+tr.Filename]
		}
		if !ok {
			// In our DB, gone from slskd: lost.
			_ = r.store.UpdateTransferProgress(ctx, tr.ID, core.TransferErrored, tr.BytesDone, tr.BytesTotal, now)
			stats.Lost++
			continue
		}
		ourIDs[lt.ID] = true
		newState := mapSlskdState(lt.State)
		_ = r.store.UpdateTransferProgress(ctx, tr.ID, newState, lt.BytesTransferred, lt.Size, now)
		switch newState {
		case core.TransferCompleted:
			stats.Completed++
		default:
			stats.Adopted++
		}
	}

	// Deadline enforcement: cancel overdue transfers in slskd.
	overdue, err := r.store.TransfersPastDeadline(ctx, now)
	if err != nil {
		return stats, err
	}
	for _, tr := range overdue {
		if tr.SlskdID != "" {
			_ = r.peers.Cancel(ctx, tr.Username, tr.SlskdID)
		}
		_ = r.store.UpdateTransferProgress(ctx, tr.ID, core.TransferCancelled, tr.BytesDone, tr.BytesTotal, now)
		stats.Cancelled++
	}

	// Count transfers slskd knows about that are not ours (left untouched).
	for _, t := range live {
		if !ourIDs[t.ID] {
			stats.Unknown++
		}
	}
	return stats, nil
}
