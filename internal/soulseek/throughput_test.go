package soulseek

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestThroughputMeterRingKeepsLastNOldestFirst(t *testing.T) {
	m := newThroughputMeter()
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	// Record more than throughputWindow samples, one per second, each within
	// the same minute so no minute roll interferes with the ring assertion.
	for i := 0; i < throughputWindow+10; i++ {
		m.record(base.Add(time.Duration(i)*time.Millisecond), int64(i), 1)
	}
	samples := m.Samples()
	if len(samples) != throughputWindow {
		t.Fatalf("ring length = %d, want %d", len(samples), throughputWindow)
	}
	// The ring keeps the most recent throughputWindow samples, oldest first:
	// the first 10 (bps 0..9) fell off, so the oldest remaining is bps 10.
	if samples[0].BytesPerSecond != 10 {
		t.Errorf("oldest sample bps = %d, want 10", samples[0].BytesPerSecond)
	}
	last := int64(throughputWindow + 10 - 1)
	if got := samples[len(samples)-1].BytesPerSecond; got != last {
		t.Errorf("newest sample bps = %d, want %d", got, last)
	}
}

func TestThroughputMeterSamplesNeverNil(t *testing.T) {
	m := newThroughputMeter()
	samples := m.Samples()
	if samples == nil {
		t.Fatal("Samples() returned nil, want non-nil empty slice")
	}
	if len(samples) != 0 {
		t.Fatalf("expected 0 samples, got %d", len(samples))
	}
}

func TestThroughputMeterMinuteRollComputesAvgMaxAndSamples(t *testing.T) {
	m := newThroughputMeter()
	minute1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	m.record(minute1.Add(0*time.Second), 100, 1)
	m.record(minute1.Add(20*time.Second), 200, 3)
	m.record(minute1.Add(40*time.Second), 300, 2)

	// Crossing into the next minute closes the first minute's accumulator.
	minute2 := minute1.Add(time.Minute)
	m.record(minute2, 50, 1)

	got := m.TakeThroughputMinutes(minute2, false)
	if len(got) != 1 {
		t.Fatalf("pending minutes = %d, want 1", len(got))
	}
	want := core.ThroughputMinute{
		Minute: minute1, AvgBytesPerSecond: 200, MaxBytesPerSecond: 300, MaxActive: 3, Samples: 3,
	}
	if got[0] != want {
		t.Errorf("closed minute = %+v, want %+v", got[0], want)
	}
}

func TestThroughputMeterIdleMinuteProducesNoRowButZerosEnterRing(t *testing.T) {
	m := newThroughputMeter()
	minute1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// An entirely idle minute: zero bytes, zero active transfers throughout.
	m.record(minute1.Add(0*time.Second), 0, 0)
	m.record(minute1.Add(30*time.Second), 0, 0)

	minute2 := minute1.Add(time.Minute)
	m.record(minute2, 0, 0)

	got := m.TakeThroughputMinutes(minute2, false)
	if len(got) != 0 {
		t.Fatalf("idle minute produced %d pending rows, want 0: %+v", len(got), got)
	}

	// The zero samples still entered the ring.
	samples := m.Samples()
	if len(samples) != 3 {
		t.Fatalf("ring length = %d, want 3", len(samples))
	}
}

func TestThroughputMeterPartialMinuteTakeOnShutdownThenEmptyOnSecondCall(t *testing.T) {
	m := newThroughputMeter()
	minute1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(minute1.Add(0*time.Second), 100, 1)
	m.record(minute1.Add(10*time.Second), 200, 1)

	now := minute1.Add(15 * time.Second)
	got := m.TakeThroughputMinutes(now, true)
	if len(got) != 1 {
		t.Fatalf("shutdown drain = %d minutes, want 1: %+v", len(got), got)
	}
	if got[0].Samples != 2 {
		t.Errorf("partial minute samples = %d, want 2", got[0].Samples)
	}

	// A second call with nothing new recorded finds nothing pending.
	again := m.TakeThroughputMinutes(now, true)
	if len(again) != 0 {
		t.Fatalf("second drain = %d minutes, want 0: %+v", len(again), again)
	}
}

func TestThroughputMeterPendingBoundedDropsOldest(t *testing.T) {
	m := newThroughputMeter()
	base := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	// Close throughputPendingCap+3 minutes, each with one nonzero sample so
	// every one of them is actually enqueued onto pending.
	for i := 0; i < throughputPendingCap+3; i++ {
		minute := base.Add(time.Duration(i) * time.Minute)
		m.record(minute, int64(i+1), 1)
	}
	// Roll into one more minute to close the last of the loop's minutes.
	m.record(base.Add(time.Duration(throughputPendingCap+3)*time.Minute), 1, 1)

	got := m.TakeThroughputMinutes(time.Time{}, false)
	if len(got) != throughputPendingCap {
		t.Fatalf("pending length = %d, want %d (bounded, oldest dropped)", len(got), throughputPendingCap)
	}
	// The oldest surviving minute is minute index 3 (0,1,2 dropped for cap 10
	// out of 13 produced).
	wantOldest := base.Add(3 * time.Minute)
	if !got[0].Minute.Equal(wantOldest) {
		t.Errorf("oldest surviving minute = %v, want %v", got[0].Minute, wantOldest)
	}
}

func TestThroughputMeterSubscribersNotified(t *testing.T) {
	m := newThroughputMeter()
	var got []core.ThroughputSample
	m.Subscribe(func(s core.ThroughputSample) {
		got = append(got, s)
	})

	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(at, 100, 1)
	m.record(at.Add(time.Second), 200, 2)

	if len(got) != 2 {
		t.Fatalf("subscriber notified %d times, want 2", len(got))
	}
	if got[0].BytesPerSecond != 100 || got[1].BytesPerSecond != 200 {
		t.Errorf("subscriber samples = %+v, want bps 100 then 200", got)
	}
}

func TestThroughputTickSumsByteDeltasAndCountsFrozenTransferActive(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())

	moving := newTransfer("moving", "alice", "a.flac", 1000)
	moving.state = core.TransferInProgress
	moving.bytesDone.Store(100)
	c.downloads.insert(moving)

	frozen := newTransfer("frozen", "bob", "b.flac", 1000)
	frozen.state = core.TransferInProgress
	frozen.bytesDone.Store(500)
	c.downloads.insert(frozen)

	queued := newTransfer("queued", "carol", "c.flac", 1000)
	queued.state = core.TransferQueued
	c.downloads.insert(queued)

	last := map[string]int64{"moving": 100, "frozen": 500, "queued": 0}
	// Advance moving's bytesDone; frozen and queued stay put.
	moving.bytesDone.Store(600)

	bps, active := c.throughputTick(time.Now(), last)
	if bps != 500 {
		// throughputInterval defaults to 1s in New(), so 500 bytes / 1s = 500 bps.
		t.Errorf("bps = %d, want 500", bps)
	}
	if active != 2 {
		t.Errorf("active = %d, want 2 (moving + frozen, both TransferInProgress)", active)
	}
	if last["frozen"] != 500 {
		t.Errorf("frozen's tracked bytes = %d, want unchanged 500", last["frozen"])
	}
}

func TestThroughputTickEvictsTransfersNoLongerPresent(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	last := map[string]int64{"gone": 1234, "also-gone": 5678}

	c.throughputTick(time.Now(), last)

	if len(last) != 0 {
		t.Errorf("last map after tick = %+v, want empty (both entries evicted)", last)
	}
}

func TestSampleThroughputReturnsPromptlyOnCancel(t *testing.T) {
	cfg := Config{Address: "unused:0", Username: "me", Password: "p"}
	cfg.throughputInterval = time.Millisecond
	c := New(cfg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.sampleThroughput(ctx)
		close(done)
	}()

	// Let it tick at least once, then cancel and expect a prompt return.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sampleThroughput did not return promptly after ctx cancellation")
	}
}
