package main

import (
	"context"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/soulseek"
)

// shareMetaStore is the narrow slice of *store.Store shareMetaCache needs, so
// tests can supply a fake rather than a real database.
type shareMetaStore interface {
	ShareFileMetadata(ctx context.Context) ([]core.ShareFileMeta, error)
	UpsertShareFileMetadata(ctx context.Context, entries []core.ShareFileMeta, now time.Time) error
	DeleteShareFileMetadata(ctx context.Context, paths []string) error
}

// shareMetaCache adapts shareMetaStore to soulseek.ShareMetaCache (issue #197),
// translating between the two packages' otherwise-identical ShareFileMeta types
// so internal/store never has to import internal/soulseek.
type shareMetaCache struct {
	store shareMetaStore // narrow interface, for tests
}

// LoadShareMeta implements soulseek.ShareMetaCache.
func (c *shareMetaCache) LoadShareMeta(ctx context.Context) ([]soulseek.ShareFileMeta, error) {
	rows, err := c.store.ShareFileMetadata(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]soulseek.ShareFileMeta, len(rows))
	for i, r := range rows {
		out[i] = soulseek.ShareFileMeta{Path: r.Path, Size: r.Size, ModTime: r.ModTime, Bitrate: r.Bitrate, Duration: r.Duration}
	}
	return out, nil
}

// SaveShareMeta implements soulseek.ShareMetaCache. It upserts before it
// deletes: the upsert is the actual cached value this scan computed, the
// delete is hygiene pruning stale rows, so a delete failure must never
// discard a successful upsert. The delete step runs even after an upsert
// error (so pruning is never blocked by an unrelated upsert failure); it is
// skipped entirely when there is nothing to prune. The first error, if any,
// is returned.
func (c *shareMetaCache) SaveShareMeta(ctx context.Context, upserts []soulseek.ShareFileMeta, stalePaths []string) error {
	entries := make([]core.ShareFileMeta, len(upserts))
	for i, u := range upserts {
		entries[i] = core.ShareFileMeta{Path: u.Path, Size: u.Size, ModTime: u.ModTime, Bitrate: u.Bitrate, Duration: u.Duration}
	}

	upsertErr := c.store.UpsertShareFileMetadata(ctx, entries, time.Now())
	var deleteErr error
	if len(stalePaths) > 0 {
		deleteErr = c.store.DeleteShareFileMetadata(ctx, stalePaths)
	}
	if upsertErr != nil {
		return upsertErr
	}
	return deleteErr
}
