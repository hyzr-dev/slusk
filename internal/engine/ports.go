// Package engine owns the scheduler, workers, and reconciler. It defines the
// narrow port interfaces it consumes here (Go style: the consumer declares the
// interface), so the concrete slskd/store types satisfy them implicitly.
package engine

import (
	"context"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// PeerNetwork is the slice of the slskd client the engine needs.
type PeerNetwork interface {
	ListDownloads(ctx context.Context) ([]slskd.Transfer, error)
	Cancel(ctx context.Context, username, id string) error
}

// JobStore is the slice of the store the reconciler needs.
type JobStore interface {
	ActiveTransfers(ctx context.Context) ([]core.Transfer, error)
	TransfersPastDeadline(ctx context.Context, now time.Time) ([]core.Transfer, error)
	UpdateTransferProgress(ctx context.Context, id int64, state core.TransferState, done, total int64, now time.Time) error
}
