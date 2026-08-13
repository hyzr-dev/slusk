package main

import (
	"context"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/soulseek"
)

// shareIndexStore is the narrow slice of *store.Store shareIndexAdapter needs,
// so tests can supply a fake rather than a real database.
type shareIndexStore interface {
	ShareIndex(ctx context.Context) (*core.ShareIndex, error)
	ReplaceShareIndex(ctx context.Context, index core.ShareIndex) error
}

// shareIndexAdapter adapts shareIndexStore to soulseek.ShareIndexStore (issue
// #497), translating between the two packages' otherwise-identical share index
// types so internal/store never has to import internal/soulseek.
type shareIndexAdapter struct {
	store shareIndexStore // narrow interface, for tests
}

// LoadShareIndex implements soulseek.ShareIndexStore. A nil index (nothing
// stored yet) is passed through as nil rather than an empty one: an empty index
// would mean "the last scan found no files", which is a different thing.
func (a *shareIndexAdapter) LoadShareIndex(ctx context.Context) (*soulseek.ShareIndex, error) {
	stored, err := a.store.ShareIndex(ctx)
	if err != nil || stored == nil {
		return nil, err
	}
	index := &soulseek.ShareIndex{
		ScannedAt:    stored.ScannedAt,
		ScanDuration: stored.ScanDuration,
		Folders:      make([]soulseek.SharedFolder, len(stored.Folders)),
		Directories:  stored.Directories,
		Files:        make([]soulseek.ShareIndexEntry, len(stored.Files)),
		FileCount:    stored.FileCount,
		TotalBytes:   stored.TotalBytes,
	}
	for i, f := range stored.Folders {
		index.Folders[i] = soulseek.SharedFolder{Name: f.Name, Path: f.Path}
	}
	for i, e := range stored.Files {
		index.Files[i] = soulseek.ShareIndexEntry{
			VirtualPath: e.VirtualPath, LocalPath: e.LocalPath, ShareRoot: e.ShareRoot,
			Size: e.Size, Extension: e.Extension, ModTime: e.ModTime,
			Bitrate: e.Bitrate, Duration: e.Duration,
		}
	}
	return index, nil
}

// SaveShareIndex implements soulseek.ShareIndexStore. FileCount is not carried
// across: the store derives it from the rows it actually writes, so the two can
// never disagree.
func (a *shareIndexAdapter) SaveShareIndex(ctx context.Context, index *soulseek.ShareIndex) error {
	stored := core.ShareIndex{
		ScannedAt:    index.ScannedAt,
		ScanDuration: index.ScanDuration,
		Folders:      make([]core.SharedFolder, len(index.Folders)),
		Directories:  index.Directories,
		Files:        make([]core.ShareIndexEntry, len(index.Files)),
		TotalBytes:   index.TotalBytes,
	}
	for i, f := range index.Folders {
		stored.Folders[i] = core.SharedFolder{Name: f.Name, Path: f.Path}
	}
	for i, e := range index.Files {
		stored.Files[i] = core.ShareIndexEntry{
			VirtualPath: e.VirtualPath, LocalPath: e.LocalPath, ShareRoot: e.ShareRoot,
			Size: e.Size, Extension: e.Extension, ModTime: e.ModTime,
			Bitrate: e.Bitrate, Duration: e.Duration,
		}
	}
	return a.store.ReplaceShareIndex(ctx, stored)
}
