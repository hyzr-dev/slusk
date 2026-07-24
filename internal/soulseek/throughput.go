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

	subs []func(core.ThroughputSample)
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

// enqueuePending appends min to pending, dropping the oldest entry first if
// that would exceed throughputPendingCap. Must be called with mu held.
func (m *throughputMeter) enqueuePending(min core.ThroughputMinute) {
	if len(m.pending) >= throughputPendingCap {
		m.pending = m.pending[1:]
	}
	m.pending = append(m.pending, min)
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
// mid-minute. now is unused when includePartial is false; it exists so the
// caller does not need a separate no-op branch.
func (m *throughputMeter) TakeThroughputMinutes(now time.Time, includePartial bool) []core.ThroughputMinute {
	m.mu.Lock()
	defer m.mu.Unlock()

	if includePartial && m.minuteSet {
		m.closeMinute()
	}
	out := m.pending
	m.pending = nil
	return out
}

// Subscribe registers fn to be called, under the lock, with every future
// sample record() takes. This is the seam for a future SSE live-throughput
// stream (issue #157 lays the groundwork but wires no production caller
// yet — see throughput_test.go for the pinning test that keeps it from
// rotting unused). fn MUST NOT block: it runs synchronously inside record(),
// so a slow or blocking subscriber would stall every future sample.
func (m *throughputMeter) Subscribe(fn func(core.ThroughputSample)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

// sampleThroughput owns the ticker that periodically aggregates every
// tracked download's byte-throughput into the client's throughputMeter
// (issue #157). It runs for the lifetime of the client's Run context (see
// the fourth startTracked call in Run) and returns promptly once ctx is
// cancelled.
func (c *Client) sampleThroughput(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.throughputInterval)
	defer ticker.Stop()

	last := make(map[string]int64)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			bps, active := c.throughputTick(now, last)
			c.throughput.record(now, bps, active)
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
func (c *Client) throughputTick(now time.Time, last map[string]int64) (bps int64, active int) {
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

	elapsed := c.cfg.throughputInterval.Seconds()
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
func (c *Client) TakeThroughputMinutes(now time.Time, includePartial bool) []core.ThroughputMinute {
	return c.throughput.TakeThroughputMinutes(now, includePartial)
}
