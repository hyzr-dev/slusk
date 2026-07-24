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

// throughputMeter aggregates the native soulseek client's per-second
// download-throughput samples (issue #157) into a short in-memory ring (for
// the live sparkline) and a queue of completed per-minute rollups (for
// persistence). All time comes from the caller (record's at parameter)
// rather than time.Now(), so tests need no clock abstraction — see
// sampleThroughput in client.go for the only production caller.
type throughputMeter struct {
	mu sync.Mutex

	ring     [throughputWindow]core.ThroughputSample
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
	// id private to this meter so Unsubscribe (the returned cancel func) can
	// remove exactly one subscriber without disturbing the others. A map
	// rather than a slice because the intended consumer is an SSE stream
	// where clients connect and disconnect constantly (issue #157 F6): an
	// append-only slice would leave every disconnected client's closure
	// invoked every second forever, in a daemon that runs for months.
	subs      map[uint64]func(core.ThroughputSample)
	nextSubID uint64
}

// newThroughputMeter constructs an empty throughputMeter.
func newThroughputMeter() *throughputMeter {
	return &throughputMeter{}
}

// record folds one sample (bps aggregate bytes/sec, active transfer count) at
// instant at into the ring and the in-flight minute accumulator, rolling the
// minute and enqueueing it onto pending when at crosses into a new UTC minute
// (see closeMinute). Every subscriber registered via Subscribe is notified,
// under the lock — see Subscribe's doc comment on why that means a
// subscriber must never block.
func (m *throughputMeter) record(at time.Time, bps int64, active int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sample := core.ThroughputSample{At: at, BytesPerSecond: bps, ActiveTransfers: active}
	m.ring[m.ringNext] = sample
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
	m.sumBytes += bps
	if bps > m.peakBytes {
		m.peakBytes = bps
	}
	if active > m.maxActive {
		m.maxActive = active
	}
	m.samples++

	for _, fn := range m.subs {
		fn(sample)
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

// Samples returns a copy of every sample currently in the ring, oldest
// first. Never nil, so JSON-encoding it always serializes to "[]" rather
// than "null" (see internal/observ/charts.go's Throughput field).
func (m *throughputMeter) Samples() []core.ThroughputSample {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]core.ThroughputSample, 0, m.ringLen)
	start := (m.ringNext - m.ringLen + throughputWindow) % throughputWindow
	for i := 0; i < m.ringLen; i++ {
		out = append(out, m.ring[(start+i)%throughputWindow])
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
// Invariant: minuteSet is true only while the accumulator genuinely holds
// live data for m.minute. A partial close reports and zeroes that
// accumulator, so minuteSet is cleared here too — otherwise a later record()
// landing in the same still-open wall-clock minute (includePartial is only
// ever true today at final shutdown, right before the process exits, but
// nothing enforces that a future caller won't invoke this mid-run) would
// silently resume accumulating under a minute the caller already believes it
// has drained, and a subsequent close of that minute would upsert over the
// already-reported row rather than genuinely extending it (issue #157 F5).
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
func (m *throughputMeter) Subscribe(fn func(core.ThroughputSample)) (cancel func()) {
	m.mu.Lock()
	id := m.nextSubID
	m.nextSubID++
	if m.subs == nil {
		m.subs = make(map[uint64]func(core.ThroughputSample))
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

// sampleThroughput owns the ticker that periodically aggregates every
// tracked download's byte-throughput into the client's throughputMeter
// (issue #157). It runs for the lifetime of the client's Run context (see
// the fourth startTracked call in Run) and returns promptly once ctx is
// cancelled. prevAt tracks the wall-clock time of the previous tick so
// throughputTick can divide by the real elapsed interval rather than the
// ticker's configured one — see throughputTick's doc comment.
func (c *Client) sampleThroughput(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.throughputInterval)
	defer ticker.Stop()

	last := make(map[string]int64)
	var prevAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			bps, active := c.throughputTick(now, prevAt, last)
			c.throughput.record(now, bps, active)
			prevAt = now
		}
	}
}

// throughputTick computes one aggregate throughput sample from the byte
// deltas of every tracked transfer since the previous tick, reading
// bytesDone atomically (no mutex) rather than summing each transfer's
// tr.speed field: a stalled transfer's tr.speed can be nonzero (see the
// stale-speed fix in ListDownloads) for up to speedStaleAfter, but its
// bytesDone delta is genuinely zero the instant it stalls, so this
// contributes 0 for it automatically without needing to know about
// staleness at all. active counts every transfer in core.TransferInProgress
// regardless of whether it moved bytes this tick — a stalled transfer is
// still occupying a slot. last is mutated in place: entries for transfers no
// longer present in the current snapshot are evicted so the map cannot grow
// unbounded as downloads complete and are Remove()d over a long-running
// process.
//
// prevAt is the previous tick's timestamp (the zero Time on the very first
// tick, which has no baseline to measure from and so reports bps 0). The
// rate is total bytes moved divided by now.Sub(prevAt) — the MEASURED
// elapsed time, not c.cfg.throughputInterval: time.Ticker drops ticks under
// scheduling delay rather than queueing them (a real condition in the
// CPU-limited container this runs in, see issues #138/#139), so dividing by
// the nominal interval instead of the real one would inflate the reported
// rate by however many ticks were dropped.
func (c *Client) throughputTick(now, prevAt time.Time, last map[string]int64) (bps int64, active int) {
	snapshot := c.downloads.snapshot()

	seen := make(map[string]struct{}, len(snapshot))
	var total int64
	for _, tr := range snapshot {
		tr.mu.Lock()
		state := tr.state
		tr.mu.Unlock()
		done := tr.bytesDone.Load()

		seen[tr.id] = struct{}{}
		if prev, ok := last[tr.id]; ok {
			if delta := done - prev; delta > 0 {
				total += delta
			}
		}
		last[tr.id] = done

		if state == core.TransferInProgress {
			active++
		}
	}
	for id := range last {
		if _, ok := seen[id]; !ok {
			delete(last, id)
		}
	}

	if prevAt.IsZero() {
		return 0, active
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return 0, active
	}
	return int64(float64(total) / elapsed), active
}

// ThroughputSamples returns the client's recent aggregate download-throughput
// samples, oldest first (issue #157). Backs the Overview view's live
// sparkline (GET /api/charts).
func (c *Client) ThroughputSamples() []core.ThroughputSample {
	return c.throughput.Samples()
}

// TakeThroughputMinutes drains and returns every completed per-minute
// throughput rollup accumulated since the last call, oldest first (issue
// #157). See throughputMeter.TakeThroughputMinutes for includePartial's
// shutdown-flush semantics.
func (c *Client) TakeThroughputMinutes(includePartial bool) []core.ThroughputMinute {
	return c.throughput.TakeThroughputMinutes(includePartial)
}
