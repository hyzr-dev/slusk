package soulseek

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

func TestThroughputMeterRingKeepsAlignedLastNOldestFirst(t *testing.T) {
	m := newThroughputMeter()
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	for i := 0; i < throughputWindow+10; i++ {
		m.record(base.Add(time.Duration(i)*time.Millisecond), int64(i), 1, int64(i*2), 2)
	}

	series := m.Samples()
	if len(series.Download) != throughputWindow || len(series.Upload) != throughputWindow {
		t.Fatalf("ring lengths = download:%d upload:%d, want %d each", len(series.Download), len(series.Upload), throughputWindow)
	}
	if series.Download[0].BytesPerSecond != 10 || series.Upload[0].BytesPerSecond != 20 {
		t.Errorf("oldest pair = download:%d upload:%d, want 10/20", series.Download[0].BytesPerSecond, series.Upload[0].BytesPerSecond)
	}
	last := len(series.Download) - 1
	if series.Download[last].At != series.Upload[last].At {
		t.Errorf("newest timestamps are not aligned: %v != %v", series.Download[last].At, series.Upload[last].At)
	}
	if got, want := series.Upload[last].BytesPerSecond, int64((throughputWindow+9)*2); got != want {
		t.Errorf("newest upload bps = %d, want %d", got, want)
	}
}

func TestThroughputMeterSamplesNeverNil(t *testing.T) {
	series := newThroughputMeter().Samples()
	if series.Download == nil || series.Upload == nil {
		t.Fatalf("Samples() returned nil slice: %+v", series)
	}
	if len(series.Download) != 0 || len(series.Upload) != 0 {
		t.Fatalf("expected empty series, got %+v", series)
	}
}

func TestThroughputMeterMinuteRollComputesDownloadOnly(t *testing.T) {
	m := newThroughputMeter()
	minute1 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(minute1, 100, 1, 10_000, 7)
	m.record(minute1.Add(20*time.Second), 200, 3, 20_000, 8)
	m.record(minute1.Add(40*time.Second), 300, 2, 30_000, 9)
	m.record(minute1.Add(time.Minute), 50, 1, 40_000, 10)

	got := m.TakeThroughputMinutes(false)
	want := core.ThroughputMinute{Minute: minute1, AvgBytesPerSecond: 200, MaxBytesPerSecond: 300, MaxActive: 3, Samples: 3}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("closed minutes = %+v, want %+v", got, want)
	}
}

func TestThroughputMeterUploadOnlyMinuteIsNotPersisted(t *testing.T) {
	m := newThroughputMeter()
	minute := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(minute, 0, 0, 1000, 4)
	m.record(minute.Add(30*time.Second), 0, 0, 2000, 3)
	m.record(minute.Add(time.Minute), 0, 0, 0, 0)

	if got := m.TakeThroughputMinutes(false); len(got) != 0 {
		t.Fatalf("upload-only minute produced persisted download rows: %+v", got)
	}
	series := m.Samples()
	if len(series.Upload) != 3 || series.Upload[0].BytesPerSecond != 1000 {
		t.Fatalf("upload samples were not retained in memory: %+v", series.Upload)
	}
}

func TestThroughputMeterPartialMinuteTakeOnShutdownThenEmpty(t *testing.T) {
	m := newThroughputMeter()
	minute := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(minute, 100, 1, 200, 1)
	m.record(minute.Add(10*time.Second), 200, 1, 300, 1)

	got := m.TakeThroughputMinutes(true)
	if len(got) != 1 || got[0].Samples != 2 {
		t.Fatalf("shutdown drain = %+v, want one minute with two download samples", got)
	}
	if again := m.TakeThroughputMinutes(true); len(again) != 0 {
		t.Fatalf("second drain = %+v, want empty", again)
	}
}

func TestThroughputMeterResumesSameMinuteAfterPartialDrain(t *testing.T) {
	m := newThroughputMeter()
	minute := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(minute.Add(time.Second), 100, 1, 0, 0)
	if got := m.TakeThroughputMinutes(true); len(got) != 1 || got[0].Samples != 1 {
		t.Fatalf("first partial drain = %+v", got)
	}
	m.record(minute.Add(2*time.Second), 200, 1, 0, 0)
	m.record(minute.Add(time.Minute), 300, 1, 0, 0)
	got := m.TakeThroughputMinutes(false)
	if len(got) != 1 || got[0].Minute != minute || got[0].Samples != 1 {
		t.Fatalf("resumed minute = %+v, want same minute with one new sample", got)
	}
}

func TestThroughputMeterPendingBoundedDropsOldest(t *testing.T) {
	m := newThroughputMeter()
	base := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	for i := 0; i < throughputPendingCap+3; i++ {
		m.record(base.Add(time.Duration(i)*time.Minute), int64(i+1), 1, 0, 0)
	}
	m.record(base.Add(time.Duration(throughputPendingCap+3)*time.Minute), 1, 1, 0, 0)
	got := m.TakeThroughputMinutes(false)
	if len(got) != throughputPendingCap || got[0].Minute != base.Add(3*time.Minute) {
		t.Fatalf("bounded pending = %+v, want cap %d starting at minute 3", got, throughputPendingCap)
	}
}

func TestThroughputMeterSubscribersReceiveAlignedPairsAndCancel(t *testing.T) {
	m := newThroughputMeter()
	var got []core.ThroughputSeries
	cancel := m.Subscribe(func(series core.ThroughputSeries) { got = append(got, series) })
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m.record(at, 100, 1, 200, 2)
	cancel()
	cancel()
	m.record(at.Add(time.Second), 300, 3, 400, 4)

	if len(got) != 1 || len(got[0].Download) != 1 || len(got[0].Upload) != 1 {
		t.Fatalf("subscriber values = %+v, want one paired notification", got)
	}
	if got[0].Download[0].At != got[0].Upload[0].At || got[0].Upload[0].BytesPerSecond != 200 {
		t.Fatalf("subscriber pair is not aligned: %+v", got[0])
	}
}

func TestThroughputTickFirstTickEstablishesBothBaselines(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	tr := newTransfer("t", "alice", "a.flac", 100_000)
	tr.state = core.TransferInProgress
	tr.bytesDone.Store(1000)
	c.downloads.insert(tr)
	c.uploads.totalWritten.Store(5000)
	promoteThroughputActive(c.uploads, 3)

	baseline := throughputBaseline{downloads: make(map[string]int64)}
	download, upload := c.throughputTick(time.Now(), time.Time{}, &baseline)
	if download.BytesPerSecond != 0 || upload.BytesPerSecond != 0 {
		t.Fatalf("first rates = down:%d up:%d, want zero baselines", download.BytesPerSecond, upload.BytesPerSecond)
	}
	if download.ActiveTransfers != 1 || upload.ActiveTransfers != 3 {
		t.Fatalf("first active = down:%d up:%d, want 1/3", download.ActiveTransfers, upload.ActiveTransfers)
	}
	if baseline.downloads["t"] != 1000 || baseline.uploadBytes != 5000 {
		t.Fatalf("baseline = %+v, want download 1000 upload 5000", baseline)
	}
}

func TestThroughputTickDividesBothDirectionsByMeasuredElapsed(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	tr := newTransfer("t", "alice", "a.flac", 100_000)
	tr.state = core.TransferInProgress
	tr.bytesDone.Store(16_000)
	c.downloads.insert(tr)
	c.uploads.totalWritten.Store(31_000)
	baseline := throughputBaseline{downloads: map[string]int64{"t": 1000}, uploadBytes: 1000}
	now := time.Now()

	download, upload := c.throughputTick(now, now.Add(-3*time.Second), &baseline)
	if download.BytesPerSecond != 5000 || upload.BytesPerSecond != 10_000 {
		t.Fatalf("measured rates = down:%d up:%d, want 5000/10000", download.BytesPerSecond, upload.BytesPerSecond)
	}
}

func TestThroughputTickStallsReportZeroButRemainActive(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	tr := newTransfer("frozen", "alice", "a.flac", 1000)
	tr.state = core.TransferInProgress
	tr.bytesDone.Store(500)
	c.downloads.insert(tr)
	promoteThroughputActive(c.uploads, 2)
	baseline := throughputBaseline{downloads: map[string]int64{"frozen": 500}}
	now := time.Now()

	download, upload := c.throughputTick(now, now.Add(-time.Second), &baseline)
	if download.BytesPerSecond != 0 || upload.BytesPerSecond != 0 || download.ActiveTransfers != 1 || upload.ActiveTransfers != 2 {
		t.Fatalf("stall sample = down:%+v up:%+v", download, upload)
	}
}

func TestThroughputTickUploadTotalSurvivesCompletionBetweenTicks(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	baseline := throughputBaseline{}
	first := time.Now()
	c.throughputTick(first, time.Time{}, &baseline)

	// Model writes from two jobs followed by both jobs completing before the
	// next sample. The lifetime total preserves all bytes while active is zero.
	c.uploads.totalWritten.Add(1200)
	c.uploads.totalWritten.Add(800)
	download, upload := c.throughputTick(first.Add(2*time.Second), first, &baseline)
	if download.BytesPerSecond != 0 || upload.BytesPerSecond != 1000 || upload.ActiveTransfers != 0 {
		t.Fatalf("completion-between-ticks sample = down:%+v up:%+v, want up 1000 bps active 0", download, upload)
	}
}

func TestThroughputTickEvictsDownloadsNoLongerPresent(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	baseline := throughputBaseline{downloads: map[string]int64{"gone": 1234}}
	c.throughputTick(time.Now(), time.Now().Add(-time.Second), &baseline)
	if len(baseline.downloads) != 0 {
		t.Fatalf("download baseline after tick = %+v, want empty", baseline.downloads)
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
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sampleThroughput did not return promptly after cancellation")
	}
}

// promoteThroughputActive sets only manager bookkeeping needed to test the
// uncapped throughput active count without constructing upload jobs.
func promoteThroughputActive(m *uploadManager, active int) {
	m.mu.Lock()
	m.active = active
	m.mu.Unlock()
}
