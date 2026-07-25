package core

import "time"

// ThroughputSample is one instantaneous reading of the native soulseek
// client's aggregate download throughput (issue #157): the sum of bytes/sec
// across every in-flight download at the moment it was taken, plus how many
// downloads were active. It describes DOWNLOAD throughput only — uploads have
// no byte-level throughput tracking today, so this must not be read as
// whole-node traffic. Backs the Overview view's live sparkline (GET
// /api/charts); not persisted (see ThroughputMinute for the persisted,
// per-minute rollup).
type ThroughputSample struct {
	At              time.Time
	BytesPerSecond  int64
	ActiveTransfers int
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
