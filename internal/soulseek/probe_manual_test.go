//go:build manual

package soulseek

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// TestManualDownloadProbe exercises Client.Enqueue/ListDownloads against a
// real Soulseek server and a real peer, outside of the loopback-TCP
// simulation download_e2e_test.go exercises. It is excluded from the normal
// `go test ./...` run by the "manual" build tag and is entirely env-driven,
// so it never runs in CI.
//
// Run it with:
//
//	go test -tags manual -run TestManualDownloadProbe ./internal/soulseek -v
//
// with these environment variables set:
//
//	SLSK_SERVER  central server host:port, e.g. server.slsknet.org:2242
//	SLSK_USER    account username
//	SLSK_PASS    account password
//	SLSK_PEER    username of the peer to download from
//	SLSK_FILE    the peer's shared filename (Soulseek "\" path syntax), as
//	             returned by a real search against SLSK_PEER
//	SLSK_SIZE    that file's size in bytes (decimal), as reported by the
//	             search result
//	SLSK_DEST    local directory downloaded files are written under
//	             (Config.DownloadDir); left in place afterward - the probe
//	             does not clean it up, so the downloaded file can be
//	             inspected
func TestManualDownloadProbe(t *testing.T) {
	address := os.Getenv("SLSK_SERVER")
	username := os.Getenv("SLSK_USER")
	password := os.Getenv("SLSK_PASS")
	peerUsername := os.Getenv("SLSK_PEER")
	filename := os.Getenv("SLSK_FILE")
	sizeStr := os.Getenv("SLSK_SIZE")
	dest := os.Getenv("SLSK_DEST")
	if address == "" || username == "" || password == "" || peerUsername == "" || filename == "" || sizeStr == "" || dest == "" {
		t.Skip("SLSK_SERVER, SLSK_USER, SLSK_PASS, SLSK_PEER, SLSK_FILE, SLSK_SIZE and SLSK_DEST must all be set")
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		t.Fatalf("parse SLSK_SIZE=%q: %v", sizeStr, err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := New(Config{
		Address:     address,
		Username:    username,
		Password:    password,
		DownloadDir: dest,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Log("Run did not return within 10s of cancellation")
		}
	})

	waitForState(t, c, StateConnected, 30*time.Second)

	id, err := c.Enqueue(ctx, peerUsername, filename, size)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t.Logf("enqueued download %s: %s from %s (%d bytes)", id, filename, peerUsername, size)

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		list, err := c.ListDownloads(ctx)
		if err != nil {
			t.Fatalf("ListDownloads: %v", err)
		}
		for _, tr := range list {
			if tr.ID != id {
				continue
			}
			t.Logf("state=%s bytesDone=%d/%d queuePosition=%d speed=%d/s failure=%q retryable=%v",
				tr.State, tr.BytesDone, tr.Size, tr.QueuePosition, tr.Speed, tr.Failure, tr.Retryable)
			switch tr.State {
			case core.TransferCompleted:
				destPath := downloadDestPath(dest, filename)
				info, statErr := os.Stat(destPath)
				if statErr != nil {
					t.Fatalf("stat completed download %s: %v", destPath, statErr)
				}
				if info.Size() != size {
					t.Fatalf("completed download size = %d, want %d", info.Size(), size)
				}
				t.Logf("download completed: %s (%d bytes)", destPath, info.Size())
				return
			case core.TransferErrored:
				t.Fatalf("download errored: failure=%q retryable=%v", tr.Failure, tr.Retryable)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("timed out waiting for the download to complete")
}
