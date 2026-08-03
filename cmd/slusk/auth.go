package main

import (
	"context"
	"log/slog"
	"time"
)

// sessionPruner is the subset of app.Auth runSessionPruner needs, so this
// file only needs the method's signature rather than the whole use case.
type sessionPruner interface {
	PruneExpiredSessions(ctx context.Context) (int64, error)
}

// runSessionPruner periodically deletes expired user_sessions rows
// (issue #279), mirroring runShareRescanLoop and runThroughputRecorder's
// shape in soulseek.go. A prune failure is logged and the loop continues — a
// transient DB hiccup must not kill the daemon, and an unpruned row is
// already excluded from authenticating (SessionUser requires expires_at in
// the future) so a missed cycle has no security consequence, only a delayed
// cleanup. Unlike the throughput recorder there is nothing to flush on
// shutdown, so ctx.Done() simply returns.
func runSessionPruner(ctx context.Context, auth sessionPruner, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := auth.PruneExpiredSessions(ctx); err != nil {
				logger.Error("prune expired sessions failed", "err", err)
			} else if n > 0 {
				logger.Info("pruned expired sessions", "count", n)
			}
		}
	}
}
