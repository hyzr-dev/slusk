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

	got := m.TakeThroughputMinutes(false)
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

	got := m.TakeThroughputMinutes(false)
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

	got := m.TakeThroughputMinutes(true)
	if len(got) != 1 {
		t.Fatalf("shutdown drain = %d minutes, want 1: %+v", len(got), got)
	}
	if got[0].Samples != 2 {
		t.Errorf("partial minute samples = %d, want 2", got[0].Samples)
	}

	// A second call with nothing new recorded finds nothing pending.
	again := m.TakeThroughputMinutes(true)
	if len(again) != 0 {
		t.Fatalf("second drain = %d minutes, want 0: %+v", len(again), again)
	}
}

// TestThroughputMeterPartialCloseClearsMinuteSetForLaterSamples is issue
// #157 F5: a partial close reports and zeroes the in-flight accumulator, so
// minuteSet must be cleared too. If it weren't, a record() call landing in
// the same still-open wall-clock minute right after a partial close would
// silently resume accumulating samples the caller already believes were
// drained, and those samples alone (not the earlier-reported ones) would be
// all that a later close of that same minute reports.
func TestThroughputMeterPartialCloseClearsMinuteSetForLaterSamples(t *testing.T) {
	m := newThroughputMeter()
	minute1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(minute1.Add(1*time.Second), 100, 1)

	drained := m.TakeThroughputMinutes(true)
	if len(drained) != 1 || drained[0].Samples != 1 {
		t.Fatalf("first partial drain = %+v, want exactly one minute with 1 sample", drained)
	}

	// A record() lands within the same wall-clock minute right after the
	// partial close, then the clock genuinely rolls into the next minute.
	m.record(minute1.Add(2*time.Second), 200, 1)
	minute2 := minute1.Add(time.Minute)
	m.record(minute2, 300, 1)

	got := m.TakeThroughputMinutes(false)
	if len(got) != 1 {
		t.Fatalf("closed minutes after resumption = %d, want 1: %+v", len(got), got)
	}
	if !got[0].Minute.Equal(minute1) {
		t.Errorf("resumed minute label = %v, want %v (the same wall-clock minute the resumed sample genuinely belongs to)", got[0].Minute, minute1)
	}
	if got[0].Samples != 1 {
		t.Errorf("resumed minute samples = %d, want 1 (only the post-partial-close sample; the pre-partial-close sample was already reported and must not be double-counted)", got[0].Samples)
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

	got := m.TakeThroughputMinutes(false)
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

// TestThroughputMeterUnsubscribeStopsNotifications is issue #157 F6:
// Subscribe's cancel func must actually remove the subscriber, not just be a
// no-op — otherwise every disconnected SSE client would leave a closure
// invoked every second forever.
func TestThroughputMeterUnsubscribeStopsNotifications(t *testing.T) {
	m := newThroughputMeter()
	var got []core.ThroughputSample
	cancel := m.Subscribe(func(s core.ThroughputSample) {
		got = append(got, s)
	})

	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(at, 100, 1)
	cancel()
	m.record(at.Add(time.Second), 200, 2)

	if len(got) != 1 {
		t.Fatalf("subscriber notified %d times after cancel, want 1 (only the pre-cancel sample)", len(got))
	}
	if got[0].BytesPerSecond != 100 {
		t.Errorf("recorded sample bps = %d, want 100", got[0].BytesPerSecond)
	}
}

// TestThroughputMeterUnsubscribeSafeToCallTwice asserts cancel is idempotent:
// a second call must not panic (e.g. by deleting an already-removed map
// entry, or double-closing something).
func TestThroughputMeterUnsubscribeSafeToCallTwice(t *testing.T) {
	m := newThroughputMeter()
	cancel := m.Subscribe(func(core.ThroughputSample) {})
	cancel()
	cancel()
}

// TestThroughputMeterUnsubscribeOnlyRemovesItsOwnSubscriber asserts
// cancelling one subscription leaves other, still-live subscriptions
// notified normally.
func TestThroughputMeterUnsubscribeOnlyRemovesItsOwnSubscriber(t *testing.T) {
	m := newThroughputMeter()
	var gotA, gotB []core.ThroughputSample
	cancelA := m.Subscribe(func(s core.ThroughputSample) { gotA = append(gotA, s) })
	m.Subscribe(func(s core.ThroughputSample) { gotB = append(gotB, s) })

	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(at, 100, 1)
	cancelA()
	m.record(at.Add(time.Second), 200, 2)

	if len(gotA) != 1 {
		t.Errorf("subscriber A notified %d times, want 1 (cancelled after the first sample)", len(gotA))
	}
	if len(gotB) != 2 {
		t.Errorf("subscriber B notified %d times, want 2 (never cancelled)", len(gotB))
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

	now := time.Now()
	prevAt := now.Add(-time.Second)
	bps, active := c.throughputTick(now, prevAt, last)
	if bps != 500 {
		// 500 bytes moved over a measured 1s elapsed = 500 bps.
		t.Errorf("bps = %d, want 500", bps)
	}
	if active != 2 {
		t.Errorf("active = %d, want 2 (moving + frozen, both TransferInProgress)", active)
	}
	if last["frozen"] != 500 {
		t.Errorf("frozen's tracked bytes = %d, want unchanged 500", last["frozen"])
	}
}

func TestThroughputTickDividesByMeasuredElapsedNotConfiguredInterval(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	// throughputInterval defaults to 1s in New(), but time.Ticker drops ticks
	// under scheduling delay rather than queueing them (issues #138/#139), so
	// simulate a 3s real gap since the previous tick.
	tr := newTransfer("t", "alice", "a.flac", 100_000)
	tr.state = core.TransferInProgress
	tr.bytesDone.Store(1000)
	c.downloads.insert(tr)

	last := map[string]int64{"t": 1000}
	tr.bytesDone.Store(16000) // 15000 bytes moved across the 3s gap

	now := time.Now()
	prevAt := now.Add(-3 * time.Second)
	bps, _ := c.throughputTick(now, prevAt, last)
	if bps != 5000 {
		t.Errorf("bps = %d, want 5000 (15000 bytes / 3s measured elapsed, not / the configured 1s interval which would give 15000)", bps)
	}
}

func TestThroughputTickFirstTickHasNoBaselineAndReportsZeroRate(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	tr := newTransfer("t", "alice", "a.flac", 100_000)
	tr.state = core.TransferInProgress
	tr.bytesDone.Store(1000)
	c.downloads.insert(tr)

	// The zero Time signals no previous tick to measure elapsed time from.
	bps, active := c.throughputTick(time.Now(), time.Time{}, make(map[string]int64))
	if bps != 0 {
		t.Errorf("first tick bps = %d, want 0 (no baseline)", bps)
	}
	if active != 1 {
		t.Errorf("active = %d, want 1", active)
	}
}

func TestThroughputTickEvictsTransfersNoLongerPresent(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	last := map[string]int64{"gone": 1234, "also-gone": 5678}

	c.throughputTick(time.Now(), time.Now().Add(-time.Second), last)

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
