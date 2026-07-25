package core

import "time"

// ShareFileMeta is one shared audio file's cached technical metadata (issue
// #197): the persistence-layer counterpart of soulseek.ShareFileMeta, kept
// here rather than in internal/soulseek because internal/store talks only in
// core types and must not import internal/soulseek.
type ShareFileMeta struct {
	Path     string
	Size     int64
	ModTime  time.Time
	Bitrate  uint32
	Duration uint32
	// UpdatedAt is set by the store on upsert (when the row was last
	// recomputed); callers passing an entry in leave it zero.
	UpdatedAt time.Time
}
