package soulseek

import (
	"context"
	"sync"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// throughputWindow is how many 1-second core.ThroughputSample readings
// throughputMeter keeps in memory (48s), backing the Overview view's live
// sparkline (GET /api/charts, issue #157). It is deliberately short: the
// persisted per-minute rollup (core.ThroughputMinute, see
// store.RecordThroughputMinute) is what backs any history beyond that.
const throughputWindow = 48

// throughputPendingCap bounds how many completed core.ThroughputMinute
// rollups throughputMeter holds waiting for the recorder to drain them (see
// TakeThroughputMinutes). If the recorder falls behind for any reason, the
// oldest pending minute is dropped rather than growing unbounded — a gap in
// persisted history is preferable to unbounded memory growth in a
// long-running daemon.
const throughputPendingCap = 10

// throughputMeter aggregates the native Soulseek client's aligned per-second
// download and upload samples into a short in-memory ring. Only download
// samples feed the completed per-minute rollups retained for persistence.
// All time comes from record's at parameter so tests need no clock abstraction.
type throughputMeter struct {
	mu sync.Mutex

	ring     [throughputWindow]throughputPair
	ringLen  int
	ringNext int // next write position, wrapping

	// minute accumulates the current, not-yet-closed minute's stats. minuteSet
	// is false until the first record() call establishes which minute is
	// current.
	minute    time.Time
	minuteSet bool
	sumBytes  int64
	peakBytes int64
	maxActive int
	samples   int

	pending []core.ThroughputMinute

	// subs holds every live subscriber registered via Subscribe, keyed by an
	// id private to this meter so the cancel func Subscribe returns can
	// remove exactly one subscriber without disturbing the others. A map
	// rather than a slice because the intended consumer is an SSE stream
	// where clients connect and disconnect constantly (issue #157 F6): an
	// append-only slice would leave every disconnected client's closure
	// invoked every second forever, in a daemon that runs for months.
	subs      map[uint64]func(core.ThroughputSeries)
	nextSubID uint64
}

type throughputPair struct {
	download core.ThroughputSample
	upload   core.ThroughputSample
}

// newThroughputMeter constructs an empty throughputMeter.
func newThroughputMeter() *throughputMeter {
	return &throughputMeter{}
}

// record folds one aligned directional sample pair into the ring. The
// in-flight minute accumulator intentionally consumes only download values,
// preserving the existing download-only persistence contract. Subscribers
// are notified under the lock and therefore must not block.
func (m *throughputMeter) record(at time.Time, downloadBPS int64, downloadActive int, uploadBPS int64, uploadActive int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pair := throughputPair{
		download: core.ThroughputSample{At: at, BytesPerSecond: downloadBPS, ActiveTransfers: downloadActive},
		upload:   core.ThroughputSample{At: at, BytesPerSecond: uploadBPS, ActiveTransfers: uploadActive},
	}
	m.ring[m.ringNext] = pair
	m.ringNext = (m.ringNext + 1) % throughputWindow
	if m.ringLen < throughputWindow {
		m.ringLen++
	}

	current := at.UTC().Truncate(time.Minute)
	if !m.minuteSet {
		m.minute = current
		m.minuteSet = true
	} else if !current.Equal(m.minute) {
		m.closeMinute()
		m.minute = current
	}
	m.sumBytes += downloadBPS
	if downloadBPS > m.peakBytes {
		m.peakBytes = downloadBPS
	}
	if downloadActive > m.maxActive {
		m.maxActive = downloadActive
	}
	m.samples++

	series := core.ThroughputSeries{
		Download: []core.ThroughputSample{pair.download},
		Upload:   []core.ThroughputSample{pair.upload},
	}
	for _, fn := range m.subs {
		fn(series)
	}
}

// closeMinute finalizes the in-flight minute accumulator into a
// core.ThroughputMinute and enqueues it onto pending, unless the minute was
// entirely idle (zero bytes moved and zero active transfers throughout,
// decision #5 in issue #157) — an idle minute produces no persisted row, even
// though its zero samples still entered the ring above. Must be called with
// mu held.
func (m *throughputMeter) closeMinute() {
	if m.samples == 0 {
		return
	}
	if m.peakBytes != 0 || m.maxActive != 0 {
		avg := m.sumBytes / int64(m.samples)
		m.enqueuePending(core.ThroughputMinute{
			Minute:            m.minute,
			AvgBytesPerSecond: avg,
			MaxBytesPerSecond: m.peakBytes,
			MaxActive:         m.maxActive,
			Samples:           m.samples,
		})
	}
	m.sumBytes, m.peakBytes, m.maxActive, m.samples = 0, 0, 0, 0
}

// enqueuePending appends minute to pending, dropping the oldest entry first
// if that would exceed throughputPendingCap. Must be called with mu held.
func (m *throughputMeter) enqueuePending(minute core.ThroughputMinute) {
	if len(m.pending) >= throughputPendingCap {
		m.pending = m.pending[1:]
	}
	m.pending = append(m.pending, minute)
}

// Samples returns copies of both aligned directional rings, oldest first.
// Both slices are non-nil so their JSON representations are arrays even when
// no sample has been recorded.
func (m *throughputMeter) Samples() core.ThroughputSeries {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := core.ThroughputSeries{
		Download: make([]core.ThroughputSample, 0, m.ringLen),
		Upload:   make([]core.ThroughputSample, 0, m.ringLen),
	}
	start := (m.ringNext - m.ringLen + throughputWindow) % throughputWindow
	for i := 0; i < m.ringLen; i++ {
		pair := m.ring[(start+i)%throughputWindow]
		out.Download = append(out.Download, pair.download)
		out.Upload = append(out.Upload, pair.upload)
	}
	return out
}

// TakeThroughputMinutes drains and returns every pending completed minute,
// oldest first. When includePartial is true, the in-flight (not-yet-closed)
// minute accumulator is also closed and appended first — the shutdown path
// (see the throughput recorder's ctx.Done branch in cmd/slskdarr/soulseek.go)
// so a partial minute's samples are not silently lost when the process stops
// mid-minute.
//
// Invariant: minuteSet claims the accumulator holds live data for m.minute,
// so it is cleared after a partial close, which zeroes that data. This is
// state hygiene, not a behavioural guard: record() resumes accumulating under
// the same wall-clock minute either way, since the cleared branch re-derives
// the identical m.minute. includePartial is passed only from the recorder's
// ctx.Done branch, which returns immediately after, so a resumed minute is
// unreachable today; were a future caller to drain mid-run, the resumed row's
// lower Samples makes RecordThroughputMinute's upsert discard it rather than
// overwrite the already-reported one (see store/throughput.go).
func (m *throughputMeter) TakeThroughputMinutes(includePartial bool) []core.ThroughputMinute {
	m.mu.Lock()
	defer m.mu.Unlock()

	if includePartial && m.minuteSet {
		m.closeMinute()
		m.minuteSet = false
	}
	out := m.pending
	m.pending = nil
	return out
}

// Subscribe registers fn to be called, under the lock, with every future
// sample record() takes, and returns a cancel func that removes it. This is
// the seam for a future SSE live-throughput stream (issue #157 lays the
// groundwork but wires no production caller yet — see throughput_test.go for
// the pinning test that keeps it from rotting unused): SSE clients connect
// and disconnect constantly, so every subscriber must be individually
// removable rather than accumulating forever (F6).
//
// fn MUST NOT block: it runs synchronously inside record(), so a slow or
// blocking subscriber would stall every future sample. For the same reason,
// fn MUST NOT call cancel() (its own or any other subscriber's) or any other
// throughputMeter method that takes m.mu — record() holds m.mu for the
// entire fan-out, so calling back into the meter from within fn deadlocks.
//
// cancel is safe to call more than once (the second and later calls are
// no-ops) and safe to call concurrently with record()'s fan-out.
func (m *throughputMeter) Subscribe(fn func(core.ThroughputSeries)) (cancel func()) {
	m.mu.Lock()
	id := m.nextSubID
	m.nextSubID++
	if m.subs == nil {
		m.subs = make(map[uint64]func(core.ThroughputSeries))
	}
	m.subs[id] = fn
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs, id)
			m.mu.Unlock()
		})
	}
}

// throughputBaseline holds the cumulative counters observed on the previous
// aligned tick. Download counters remain per-transfer so completed downloads
// retain their existing eviction behavior; upload bytes use the manager's
// lifetime total so a completed and removed upload cannot disappear between
// ticks.
type throughputBaseline struct {
	downloads   map[string]int64
	uploadBytes uint64
}

// sampleThroughput owns the client's lifetime ticker and records one aligned
// download/upload pair on every tick. prevAt is the actual previous tick time,
// not the configured interval, because time.Ticker may drop delayed ticks.
func (c *Client) sampleThroughput(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.throughputInterval)
	defer ticker.Stop()

	baseline := throughputBaseline{downloads: make(map[string]int64)}
	var prevAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			download, upload := c.throughputTick(now, prevAt, &baseline)
			c.throughput.record(now, download.BytesPerSecond, download.ActiveTransfers, upload.BytesPerSecond, upload.ActiveTransfers)
			prevAt = now
		}
	}
}

// throughputTick computes one aligned directional sample pair from cumulative
// byte deltas. Download bytes retain the existing per-transfer accounting: a
// stalled transfer contributes zero bytes while still counting active, and
// entries disappear from the baseline when Remove deletes the transfer.
// Upload bytes instead come from uploadManager's manager-lifetime socket-write
// total, which survives job completion and excludes resume offsets. Queued
// uploads are excluded from the uncapped active count.
//
// The first tick establishes both baselines and reports zero rates. Later
// rates divide by measured elapsed time rather than the nominal ticker period.
func (c *Client) throughputTick(now, prevAt time.Time, baseline *throughputBaseline) (download core.ThroughputSample, upload core.ThroughputSample) {
	download.At = now
	upload.At = now
	if baseline.downloads == nil {
		baseline.downloads = make(map[string]int64)
	}

	snapshot := c.downloads.snapshot()
	seen := make(map[string]struct{}, len(snapshot))
	var downloadBytes int64
	for _, tr := range snapshot {
		tr.mu.Lock()
		state := tr.state
		tr.mu.Unlock()
		done := tr.bytesDone.Load()

		seen[tr.id] = struct{}{}
		if previous, ok := baseline.downloads[tr.id]; ok {
			if delta := done - previous; delta > 0 {
				downloadBytes += delta
			}
		}
		baseline.downloads[tr.id] = done
		if state == core.TransferInProgress {
			download.ActiveTransfers++
		}
	}
	for id := range baseline.downloads {
		if _, ok := seen[id]; !ok {
			delete(baseline.downloads, id)
		}
	}

	var uploadTotal uint64
	if c.uploads != nil {
		uploadTotal, upload.ActiveTransfers = c.uploads.throughputSnapshot()
	}
	var uploadBytes uint64
	if uploadTotal >= baseline.uploadBytes {
		uploadBytes = uploadTotal - baseline.uploadBytes
	}
	baseline.uploadBytes = uploadTotal

	if prevAt.IsZero() {
		return download, upload
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return download, upload
	}
	download.BytesPerSecond = int64(float64(downloadBytes) / elapsed)
	upload.BytesPerSecond = int64(float64(uploadBytes) / elapsed)
	return download, upload
}

// ThroughputSeries returns aligned recent download and upload samples, oldest
// first. This is the native observation seam for live throughput.
func (c *Client) ThroughputSeries() core.ThroughputSeries {
	return c.throughput.Samples()
}

// ThroughputSamples returns only the download side of ThroughputSeries. It is
// retained for compatibility with callers predating native upload sampling.
func (c *Client) ThroughputSamples() []core.ThroughputSample {
	return c.ThroughputSeries().Download
}

// TakeThroughputMinutes drains and returns every completed per-minute
// throughput rollup accumulated since the last call, oldest first (issue
// #157). See throughputMeter.TakeThroughputMinutes for includePartial's
// shutdown-flush semantics.
func (c *Client) TakeThroughputMinutes(includePartial bool) []core.ThroughputMinute {
	return c.throughput.TakeThroughputMinutes(includePartial)
}
