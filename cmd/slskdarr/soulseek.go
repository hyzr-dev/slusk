package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/soulseek"
)

func newSoulseekClient(cfg config.SoulseekConfig, downloadDir string, logger *slog.Logger) *soulseek.Client {
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
