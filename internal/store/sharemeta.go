// Package store: sharemeta.go persists the technical audio metadata a share
// scan extracts from every shared mp3/flac (issue #197), so a restart does
// not have to reopen every file. See migrations/0007_share_metadata_cache.sql
// for the schema and why it is a pure cache that can never affect what is
// advertised.
package store

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/hyzr-dev/slusk/internal/core"
)

// shareMetaBatch bounds how many rows one upsert/delete statement carries, so
// a huge share tree does not build one unbounded array parameter.
const shareMetaBatch = 5000

// maxSharePathBytes is the largest path UpsertShareFileMetadata will persist.
// It sits comfortably under Postgres's ~2704-byte btree index entry limit for
// the primary key; a longer path is silently skipped rather than erroring the
// whole batch, and is simply read again on the next scan.
const maxSharePathBytes = 1024

// ShareFileMetadata returns every cached row. Callers (see
// soulseek.ShareMetaCache) treat an error as an empty cache.
func (s *Store) ShareFileMetadata(ctx context.Context) ([]core.ShareFileMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, size, mtime_us, bitrate, duration, updated_at FROM share_file_metadata`)
	if err != nil {
		return nil, fmt.Errorf("share file metadata: %w", err)
	}
	defer rows.Close()

	var out []core.ShareFileMeta
	for rows.Next() {
		var m core.ShareFileMeta
		var mtimeUs int64
		if err := rows.Scan(&m.Path, &m.Size, &mtimeUs, &m.Bitrate, &m.Duration, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("share file metadata: scan: %w", err)
		}
		m.ModTime = time.UnixMicro(mtimeUs).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("share file metadata: %w", err)
	}
	return out, nil
}

// UpsertShareFileMetadata inserts or refreshes entries (keyed on Path),
// chunked at shareMetaBatch rows per statement. entries is deduplicated on
// Path (keeping the last occurrence) before chunking: Postgres rejects a
// single statement's ON CONFLICT DO UPDATE touching the same key twice
// ("command cannot affect row a second time"), and a duplicate path within
// one scan is otherwise possible (see
// TestScanSharesDeduplicatesOverlappingShares). An entry is silently skipped
// (the file is simply re-read on the next scan) if its path is longer than
// maxSharePathBytes, or is not valid UTF-8: on Linux a path is an arbitrary
// byte sequence, and a Latin-1-encoded filename (common in older music
// libraries) makes Postgres reject the *entire* INSERT statement with
// "invalid byte sequence for encoding UTF8", silently dropping every other
// row in the same batch along with it. Every other entry in the batch is
// still written.
func (s *Store) UpsertShareFileMetadata(ctx context.Context, entries []core.ShareFileMeta, now time.Time) error {
	if len(entries) == 0 {
		return nil
	}

	deduped := make(map[string]core.ShareFileMeta, len(entries))
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if len(e.Path) > maxSharePathBytes || !utf8.ValidString(e.Path) {
			continue
		}
		if _, exists := deduped[e.Path]; !exists {
			order = append(order, e.Path)
		}
		deduped[e.Path] = e
	}
	if len(order) == 0 {
		return nil
	}

	for start := 0; start < len(order); start += shareMetaBatch {
		end := min(start+shareMetaBatch, len(order))
		chunk := order[start:end]

		paths := make([]string, len(chunk))
		sizes := make([]int64, len(chunk))
		mtimes := make([]int64, len(chunk))
		bitrates := make([]int64, len(chunk))
		durations := make([]int64, len(chunk))
		for i, path := range chunk {
			e := deduped[path]
			paths[i] = e.Path
			sizes[i] = e.Size
			mtimes[i] = e.ModTime.UnixMicro()
			bitrates[i] = int64(e.Bitrate)
			durations[i] = int64(e.Duration)
		}

		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO share_file_metadata (path, size, mtime_us, bitrate, duration, updated_at)
			 SELECT p, sz, mt, br, du, $6
			   FROM unnest($1::text[], $2::bigint[], $3::bigint[], $4::bigint[], $5::bigint[])
			        AS input(p, sz, mt, br, du)
			 ON CONFLICT (path) DO UPDATE SET
			     size = EXCLUDED.size, mtime_us = EXCLUDED.mtime_us,
			     bitrate = EXCLUDED.bitrate, duration = EXCLUDED.duration,
			     updated_at = EXCLUDED.updated_at`,
			paths, sizes, mtimes, bitrates, durations, now); err != nil {
			return fmt.Errorf("upsert share file metadata: %w", err)
		}
	}
	return nil
}

// DeleteShareFileMetadata removes exactly the named rows, chunked at
// shareMetaBatch paths per statement.
func (s *Store) DeleteShareFileMetadata(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	for start := 0; start < len(paths); start += shareMetaBatch {
		end := min(start+shareMetaBatch, len(paths))
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM share_file_metadata WHERE path = ANY($1::text[])`,
			paths[start:end]); err != nil {
			return fmt.Errorf("delete share file metadata: %w", err)
		}
	}
	return nil
}
