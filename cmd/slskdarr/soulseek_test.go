package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek"
)

type fakeShareRescanner struct {
	calls  atomic.Int32
	active atomic.Int32
	max    atomic.Int32
}

func (f *fakeShareRescanner) RescanShares(context.Context) (soulseek.ShareStats, error) {
	active := f.active.Add(1)
	for {
		old := f.max.Load()
		if active <= old || f.max.CompareAndSwap(old, active) {
			break
		}
	}
	f.calls.Add(1)
	time.Sleep(5 * time.Millisecond)
	f.active.Add(-1)
	return soulseek.ShareStats{Files: 1}, nil
}

func TestShareRescanLoopSerializesSignalsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 3)
	fake := &fakeShareRescanner{}
	done := make(chan struct{})
	go func() {
		runShareRescanLoop(ctx, signals, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	signals <- syscall.SIGHUP
	signals <- syscall.SIGHUP
	deadline := time.Now().Add(time.Second)
	for fake.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rescan loop did not stop")
	}
	if fake.calls.Load() != 2 || fake.max.Load() != 1 {
		t.Fatalf("calls/max concurrency = %d/%d", fake.calls.Load(), fake.max.Load())
	}
}
