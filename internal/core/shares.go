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

// SharedFolder is one configured shared folder: the public name peers see and
// the private local directory behind it. Used here only as part of a persisted
// share index's validity condition (see ShareIndex.Folders).
// The JSON tags are load-bearing: this type is stored as JSON in
// share_index_scan.shared_folders, where it is meant to be readable by a person
// looking at the row.
type SharedFolder struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ShareIndexEntry is one indexed file as it is persisted (issue #497): the
// persistence-layer counterpart of soulseek.ShareIndexEntry, kept here for the
// same reason as ShareFileMeta — internal/store talks only in core types and
// must not import internal/soulseek.
type ShareIndexEntry struct {
	VirtualPath string
	LocalPath   string
	ShareRoot   string
	Size        int64
	Extension   string
	ModTime     time.Time
	Bitrate     uint32
	Duration    uint32
}

// ShareIndex is a complete persisted share index — the result of exactly one
// share scan, never a partial or merged one. Directories carries every
// directory the scan walked, including those holding no files, because those
// cannot be recovered from Files.
//
// ScannedAt and ScanDuration describe the scan that produced it and are
// reported unchanged after a load, so nothing ever claims the filesystem was
// read at boot when it was not.
type ShareIndex struct {
	ScannedAt    time.Time
	ScanDuration time.Duration
	// Folders is the shared folder set this index was scanned from — its only
	// validity condition. Stored in full rather than hashed so a rejected index
	// can be logged with the actual difference.
	Folders     []SharedFolder
	Directories []string
	Files       []ShareIndexEntry
	// FileCount is the file count recorded alongside the scan. The store sets
	// it from len(Files) on save and returns the stored value on load, so a
	// loader can tell a complete index from a truncated one; callers saving an
	// index leave it zero.
	FileCount  int
	TotalBytes uint64
}
