package core

import "time"

// ThroughputSample is one instantaneous directional reading of the native
// Soulseek client's aggregate throughput: the sum of bytes/sec across every
// in-flight transfer in that direction, plus how many transfers were active.
// Samples are memory-only; ThroughputMinute remains the download-only
// persistence contract.
type ThroughputSample struct {
	At              time.Time
	BytesPerSecond  int64
	ActiveTransfers int
}

// ThroughputSeries holds aligned download and upload throughput samples,
// oldest first. Native sampling appends one sample to each direction on the
// same tick, so corresponding entries always have the same timestamp.
type ThroughputSeries struct {
	Download []ThroughputSample
	Upload   []ThroughputSample
}

// ThroughputMinute is one persisted per-minute rollup of download throughput
// (issue #157), aggregated from the in-memory ThroughputSample ring: AvgBytesPerSecond
// and MaxBytesPerSecond summarize the minute's sampled rate, MaxActive is the
// peak (not average) number of concurrently active downloads observed during
// the minute, and Samples counts how many raw samples contributed — used by
// the store's upsert to prefer a more-complete write (see
// store.RecordThroughputMinute). A minute with zero bytes and zero active
// transfers the whole time is never recorded (see the soulseek throughput
// meter's idle-minute skip).
type ThroughputMinute struct {
	Minute            time.Time
	AvgBytesPerSecond int64
	MaxBytesPerSecond int64
	MaxActive         int
	Samples           int
}
