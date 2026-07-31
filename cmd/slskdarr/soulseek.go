package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/soulseek"
)

func newSoulseekClient(cfg config.SoulseekConfig, downloadDir string, sink soulseek.MessageSink, uploads soulseek.UploadSink, shareCache soulseek.ShareMetaCache, logger *slog.Logger) *soulseek.Client {
	folders := make([]soulseek.SharedFolder, 0, len(cfg.SharedFolders))
	for _, folder := range cfg.SharedFolders {
		folders = append(folders, soulseek.SharedFolder{Name: folder.Name, Path: folder.Path})
	}
	return soulseek.New(soulseek.Config{
		Address: cfg.ServerAddress, Username: cfg.Username, Password: cfg.Password,
		ListenAddr: cfg.ListenAddr, SharedFolders: folders, UploadSlots: cfg.UploadSlots,
		DownloadDir:               downloadDir,
		GluetunControlURL:         cfg.Gluetun.ControlURL,
		GluetunAPIKey:             cfg.Gluetun.APIKey,
		AllowPrivatePeerAddresses: cfg.AllowPrivatePeerAddresses,
		MessageSink:               sink,
		UploadSink:                uploads,
		ShareMetaCache:            shareCache,
	}, logger)
}

type shareRescanner interface {
	RescanShares(context.Context) (soulseek.ShareStats, error)
}

func runShareRescanLoop(ctx context.Context, signals <-chan os.Signal, client shareRescanner, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			stats, err := client.RescanShares(ctx)
			if err != nil {
				logger.Error("soulseek share rescan or advertisement failed", "err", err)
				continue
			}
			logger.Info("soulseek shares rescanned", "directories", stats.Directories, "files", stats.Files, "bytes", stats.TotalBytes)
		}
	}
}

// throughputRecorderShutdownFlushTimeout bounds the final drain
// runThroughputRecorder performs on shutdown (see below).
const throughputRecorderShutdownFlushTimeout = 5 * time.Second

// throughputSource is the subset of *soulseek.Client runThroughputRecorder
// drains completed per-minute throughput rollups from (issue #157).
type throughputSource interface {
	TakeThroughputMinutes(includePartial bool) []core.ThroughputMinute
}

// throughputSink is the subset of the store runThroughputRecorder writes
// drained rollups to.
type throughputSink interface {
	RecordThroughputMinute(ctx context.Context, m core.ThroughputMinute) error
}

// runThroughputRecorder periodically drains src's completed per-minute
// download-throughput rollups and persists them via sink (issue #157),
// mirroring runShareRescanLoop's shape. A sink write failure is logged and
// the loop continues — a transient DB hiccup must never kill throughput
// sampling, and the in-flight minute a failed write drops is superseded by
// the next tick's data anyway. Draining at interval well under a minute (the
// production wiring uses 30s) means pending, bounded at
// soulseek.throughputPendingCap, never nears its cap under normal operation.
//
// On ctx.Done(), it performs exactly one final drain with includePartial
// true so a minute still in flight at shutdown is not silently lost, using a
// FRESH context (not the already-cancelled ctx) bounded by
// throughputRecorderShutdownFlushTimeout — reusing ctx here would make every
// sink call fail immediately since ctx is already Done.
func runThroughputRecorder(ctx context.Context, src throughputSource, sink throughputSink, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	drain := func(ctx context.Context, includePartial bool) {
		for _, m := range src.TakeThroughputMinutes(includePartial) {
			if err := sink.RecordThroughputMinute(ctx, m); err != nil {
				logger.Error("record throughput minute failed", "minute", m.Minute, "err", err)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), throughputRecorderShutdownFlushTimeout)
			drain(shutdownCtx, true)
			cancel()
			return
		case <-ticker.C:
			drain(ctx, false)
		}
	}
}
