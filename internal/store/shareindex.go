// Package store: shareindex.go persists the share index — what peers are
// actually served — so a restart does not have to walk the filesystem again
// (issue #497). See migrations/0020_share_index.sql for the schema and why this
// is not a cache in the sense share_file_metadata is.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/hyzr-dev/slusk/internal/core"
)

// shareIndexBatch bounds how many rows one insert statement carries, so a
// million-file share does not build one unbounded array parameter.
const shareIndexBatch = 5000

// maxShareIndexPathBytes is the longest virtual or local path ReplaceShareIndex
// will persist. It sits under Postgres's ~2704-byte btree entry limit for the
// primary key on share_index_files.virtual_path.
//
// Unlike share_file_metadata, an over-long path here cannot simply be skipped:
// this table is what peers are served, so a dropped row is a file that silently
// stops being offered after the next restart. ReplaceShareIndex refuses the
// whole save instead — the index stays in memory, sharing is unaffected, and
// the next start does a full share scan.
const maxShareIndexPathBytes = 1024

// ShareIndex returns the persisted share index, or (nil, nil) when none has
// been saved yet. The caller (see soulseek.ShareIndexStore) treats both a nil
// index and an error as "run a full share scan".
//
// The returned FileCount is the count recorded with the scan, which the caller
// is expected to check against len(Files): they differ only if the tables are
// not the complete result of one scan, which no successful save can produce.
func (s *Store) ShareIndex(ctx context.Context) (*core.ShareIndex, error) {
	var (
		index      core.ShareIndex
		durationMs int64
		folders    []byte
		fileCount  int64
		totalBytes int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT scanned_at, scan_duration_ms, shared_folders, file_count, total_bytes
		   FROM share_index_scan`).
		Scan(&index.ScannedAt, &durationMs, &folders, &fileCount, &totalBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("share index scan row: %w", err)
	}
	if err := json.Unmarshal(folders, &index.Folders); err != nil {
		return nil, fmt.Errorf("share index shared folders: %w", err)
	}
	index.ScanDuration = time.Duration(durationMs) * time.Millisecond
	index.FileCount = int(fileCount)
	index.TotalBytes = uint64(totalBytes)

	dirs, err := s.db.QueryContext(ctx, `SELECT virtual_path FROM share_index_directories`)
	if err != nil {
		return nil, fmt.Errorf("share index directories: %w", err)
	}
	defer dirs.Close()
	for dirs.Next() {
		var path string
		if err := dirs.Scan(&path); err != nil {
			return nil, fmt.Errorf("share index directories: scan: %w", err)
		}
		index.Directories = append(index.Directories, path)
	}
	if err := dirs.Err(); err != nil {
		return nil, fmt.Errorf("share index directories: %w", err)
	}

	files, err := s.db.QueryContext(ctx,
		`SELECT virtual_path, local_path, share_root, size, mtime_us, extension, bitrate, duration
		   FROM share_index_files`)
	if err != nil {
		return nil, fmt.Errorf("share index files: %w", err)
	}
	defer files.Close()
	index.Files = make([]core.ShareIndexEntry, 0, index.FileCount)
	for files.Next() {
		var (
			e        core.ShareIndexEntry
			mtimeUs  int64
			bitrate  int64
			duration int64
		)
		if err := files.Scan(&e.VirtualPath, &e.LocalPath, &e.ShareRoot, &e.Size,
			&mtimeUs, &e.Extension, &bitrate, &duration); err != nil {
			return nil, fmt.Errorf("share index files: scan: %w", err)
		}
		e.ModTime = time.UnixMicro(mtimeUs).UTC()
		e.Bitrate = uint32(bitrate)
		e.Duration = uint32(duration)
		index.Files = append(index.Files, e)
	}
	if err := files.Err(); err != nil {
		return nil, fmt.Errorf("share index files: %w", err)
	}
	return &index, nil
}

// ReplaceShareIndex makes index the persisted share index, replacing whatever
// was there. Delete and insert run in one transaction because the tables'
// invariant is that they hold the result of exactly one complete scan: a reader
// must never see one scan's files under another scan's folder set.
//
// It is all-or-nothing in a second sense too. A path Postgres cannot store —
// longer than maxShareIndexPathBytes, or not valid UTF-8, which a Latin-1
// filename in an older music library really is — fails the whole save rather
// than being skipped, since a skipped row is a file that quietly stops being
// offered to peers after the next restart. The caller treats a save error as
// best-effort (the index is live in memory either way), so the cost is that the
// next start does a full share scan.
func (s *Store) ReplaceShareIndex(ctx context.Context, index core.ShareIndex) error {
	for _, e := range index.Files {
		if err := validShareIndexPath(e.VirtualPath); err != nil {
			return fmt.Errorf("replace share index: virtual path: %w", err)
		}
		if err := validShareIndexPath(e.LocalPath); err != nil {
			return fmt.Errorf("replace share index: local path: %w", err)
		}
		if err := validShareIndexPath(e.ShareRoot); err != nil {
			return fmt.Errorf("replace share index: share root: %w", err)
		}
	}
	for _, d := range index.Directories {
		if err := validShareIndexPath(d); err != nil {
			return fmt.Errorf("replace share index: directory: %w", err)
		}
	}
	if index.TotalBytes > math.MaxInt64 {
		return fmt.Errorf("replace share index: total bytes %d exceeds int64", index.TotalBytes)
	}
	folders, err := json.Marshal(nonNilFolders(index.Folders))
	if err != nil {
		return fmt.Errorf("replace share index: shared folders: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace share index: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	for _, table := range []string{"share_index_files", "share_index_directories", "share_index_scan"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("replace share index: clear %s: %w", table, err)
		}
	}

	for start := 0; start < len(index.Files); start += shareIndexBatch {
		end := min(start+shareIndexBatch, len(index.Files))
		chunk := index.Files[start:end]

		virtuals := make([]string, len(chunk))
		locals := make([]string, len(chunk))
		roots := make([]string, len(chunk))
		sizes := make([]int64, len(chunk))
		mtimes := make([]int64, len(chunk))
		extensions := make([]string, len(chunk))
		bitrates := make([]int64, len(chunk))
		durations := make([]int64, len(chunk))
		for i, e := range chunk {
			virtuals[i] = e.VirtualPath
			locals[i] = e.LocalPath
			roots[i] = e.ShareRoot
			sizes[i] = e.Size
			mtimes[i] = e.ModTime.UnixMicro()
			extensions[i] = e.Extension
			bitrates[i] = int64(e.Bitrate)
			durations[i] = int64(e.Duration)
		}
		// ON CONFLICT DO NOTHING rather than no conflict clause at all: the
		// in-memory index is keyed on the virtual path, so a duplicate cannot
		// reach here from a scan, but a duplicate would abort the entire
		// transaction and take a working index down with it.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO share_index_files
			     (virtual_path, local_path, share_root, size, mtime_us, extension, bitrate, duration)
			 SELECT vp, lp, sr, sz, mt, ex, br, du
			   FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::bigint[],
			               $6::text[], $7::bigint[], $8::bigint[])
			        AS input(vp, lp, sr, sz, mt, ex, br, du)
			 ON CONFLICT (virtual_path) DO NOTHING`,
			virtuals, locals, roots, sizes, mtimes, extensions, bitrates, durations); err != nil {
			return fmt.Errorf("replace share index: insert files: %w", err)
		}
	}

	for start := 0; start < len(index.Directories); start += shareIndexBatch {
		end := min(start+shareIndexBatch, len(index.Directories))
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO share_index_directories (virtual_path)
			 SELECT vp FROM unnest($1::text[]) AS input(vp)
			 ON CONFLICT (virtual_path) DO NOTHING`,
			index.Directories[start:end]); err != nil {
			return fmt.Errorf("replace share index: insert directories: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO share_index_scan
		     (id, scanned_at, scan_duration_ms, shared_folders, file_count, total_bytes)
		 VALUES (TRUE, $1, $2, $3, $4, $5)`,
		index.ScannedAt, index.ScanDuration.Milliseconds(), folders,
		int64(len(index.Files)), int64(index.TotalBytes)); err != nil {
		return fmt.Errorf("replace share index: insert scan row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace share index: commit: %w", err)
	}
	return nil
}

// validShareIndexPath reports why a path cannot be persisted, or nil.
func validShareIndexPath(path string) error {
	if len(path) > maxShareIndexPathBytes {
		return fmt.Errorf("%d bytes exceeds the %d-byte limit", len(path), maxShareIndexPathBytes)
	}
	if !utf8.ValidString(path) {
		return errors.New("is not valid UTF-8")
	}
	return nil
}

// nonNilFolders keeps a share index with no configured folders marshalling to
// `[]` rather than `null`, so the stored value always reads as a list.
func nonNilFolders(folders []core.SharedFolder) []core.SharedFolder {
	if folders == nil {
		return []core.SharedFolder{}
	}
	return folders
}
