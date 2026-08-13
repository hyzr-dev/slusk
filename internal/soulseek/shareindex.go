package soulseek

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

// ShareIndexEntry is one indexed file as it is persisted between runs: the
// public path it is offered under, where its bytes actually live, and the
// detail a peer is shown about it. Size and ModTime are what the upload path
// re-checks before serving bytes, so an entry loaded from a scan that ran days
// ago still refuses a file that has changed since.
type ShareIndexEntry struct {
	VirtualPath string
	LocalPath   string
	// ShareRoot is the symlink-resolved absolute root LocalPath must stay
	// beneath. Persisted rather than re-derived at load: resolving it would put
	// the filesystem back on the startup path.
	ShareRoot string
	Size      int64
	Extension string
	ModTime   time.Time
	Bitrate   uint32
	Duration  uint32
}

// ShareIndex is the complete result of one share scan, in the form it survives
// a restart in. Directories carries every directory the scan walked, including
// those holding no files of their own — those cannot be recovered from Files,
// and losing them would both shrink the folder count announced to the server
// and hide folders from a browsing peer.
type ShareIndex struct {
	// ScannedAt and ScanDuration describe the scan that produced this index and
	// are reported unchanged after a load. Nothing may claim the filesystem was
	// read at boot when it was not.
	ScannedAt    time.Time
	ScanDuration time.Duration
	// Folders is the shared folder set this index was scanned from, and the
	// index's only validity condition. It is compared in full, not hashed, so a
	// rejected index can be logged with the actual difference.
	Folders     []SharedFolder
	Directories []string
	Files       []ShareIndexEntry
	// FileCount is the file count the store recorded alongside the scan. A
	// loader compares it with len(Files) to tell a complete index from a
	// truncated one; a caller saving an index leaves it zero.
	FileCount  int
	TotalBytes uint64
}

// ShareIndexStore persists the share index — what peers are actually served —
// so a restart can reuse the last scan instead of walking the filesystem again
// (issue #497).
//
// It is deliberately a separate port from ShareMetaCache rather than more
// methods on it. ShareMetaCache is an accelerator in front of reading audio
// files, valid while a file's size and mtime match, and a miss costs only time.
// This is the artifact itself, valid while the shared folder set matches, and a
// wrong row is a wrong answer on the wire. Sharing one interface would give the
// shorter lifetime to both.
//
// Both methods are best-effort in the sense that failure is never fatal: a load
// error means a full share scan runs instead, and a save error only means the
// next start has to scan. Neither may fail a scan.
type ShareIndexStore interface {
	// LoadShareIndex returns the persisted index, or (nil, nil) when none has
	// been saved. The caller treats both a nil index and an error as "scan".
	LoadShareIndex(ctx context.Context) (*ShareIndex, error)

	// SaveShareIndex replaces the persisted index with index, atomically. The
	// stored index is always the result of exactly one complete scan — there is
	// no incremental or partial write.
	SaveShareIndex(ctx context.Context, index *ShareIndex) error
}

// loadShareIndex returns the persisted index and why it may be used, or a
// human-readable reason it may not. reason is empty exactly when index is
// non-nil.
func (c *Client) loadShareIndex(ctx context.Context) (index *ShareIndex, reason string) {
	loadCtx, cancel := context.WithTimeout(ctx, c.cfg.shareIndexTimeout)
	defer cancel()
	stored, err := c.cfg.ShareIndexStore.LoadShareIndex(loadCtx)
	if err != nil {
		return nil, fmt.Sprintf("reading the stored share index failed: %v", err)
	}
	if stored == nil {
		return nil, "no share index has been stored yet"
	}
	if diff := diffSharedFolders(stored.Folders, c.cfg.SharedFolders); diff != "" {
		return nil, "the shared folders changed since the stored share index was written: " + diff
	}
	if stored.FileCount != len(stored.Files) {
		return nil, fmt.Sprintf("the stored share index is incomplete: it records %d files but holds %d rows",
			stored.FileCount, len(stored.Files))
	}
	return stored, ""
}

// loadAndPublishShareIndex tries to publish the persisted share index instead
// of scanning the filesystem, and reports whether it did. It claims the
// share-scan slot for the same reason a scan does: it publishes a snapshot, and
// two publishers racing over which one is current is exactly what the slot
// prevents.
//
// Every path that declines to use the stored index logs why at warn level.
// A silent fallback would make "why did this restart re-read my NAS" an
// unanswerable question, which is the whole reason the folder set is stored in
// full rather than hashed.
//
// A returned error is always permanent (ErrShareTooLarge) and has already been
// recorded on the client: the same files would produce the same error from a
// scan, so the caller must not fall back to one.
func (c *Client) loadAndPublishShareIndex(ctx context.Context) (bool, error) {
	if c.cfg.ShareIndexStore == nil {
		return false, nil
	}
	if err := c.acquireShareScan(ctx); err != nil {
		return false, nil
	}
	defer c.releaseShareScan()

	index, reason := c.loadShareIndex(ctx)
	if index == nil {
		if c.logger != nil {
			c.logger.Warn("running a full share scan instead of loading the stored share index", "reason", reason)
		}
		return false, nil
	}

	snapshot, err := rebuildShareSnapshot(ctx, index)
	if err != nil {
		if errors.Is(err, ErrShareTooLarge) {
			// Permanent, exactly as from a scan: a scan of the same files would
			// build the same oversized browse frame. Retrying, or falling back
			// to a walk, would only spend the I/O to fail identically.
			c.shareFailure.Store(&shareFailure{Message: err.Error(), At: time.Now()})
			return false, err
		}
		if c.logger != nil {
			c.logger.Warn("running a full share scan instead of loading the stored share index",
				"reason", fmt.Sprintf("rebuilding the index in memory failed: %v", err))
		}
		return false, nil
	}

	c.shareFailure.Store(nil)
	c.shares.Store(snapshot)
	if c.logger != nil {
		c.logger.Info("share index loaded from the database; the filesystem was not read",
			"directories", snapshot.stats.Directories,
			"files", snapshot.stats.Files,
			"bytes", snapshot.stats.TotalBytes,
			"indexed_at", snapshot.stats.IndexedAt)
	}
	return true, nil
}

// rebuildShareSnapshot turns a persisted index back into a publishable
// snapshot: the per-folder statistics, the directory listing, the trigram
// search index and the serialised browse frame, all derived from the rows
// exactly as scanShares derives them from the filesystem. It performs no I/O.
//
// The sort order matters and is the scan's: directory and file order is what a
// browsing peer sees, and the trigram postings are positions into the sorted
// search slice.
func rebuildShareSnapshot(ctx context.Context, index *ShareIndex) (*shareSnapshot, error) {
	s := &shareSnapshot{
		files:       make(map[string]*indexedFile, len(index.Files)),
		byDirectory: make(map[string]peer.Directory, len(index.Directories)),
		stats: ShareStats{
			IndexedAt:    index.ScannedAt,
			ScanDuration: index.ScanDuration,
		},
	}
	for _, name := range index.Directories {
		if _, exists := s.byDirectory[name]; !exists {
			s.byDirectory[name] = peer.Directory{Name: name}
		}
	}

	// Folder statistics are re-aggregated rather than stored: a virtual path's
	// first segment is the public name of the shared folder it belongs to, so
	// the counts follow from the rows and cannot drift out of step with them.
	folders := make(map[string]*ShareFolderStats, len(index.Folders))
	order := make([]string, 0, len(index.Folders))
	for _, folder := range index.Folders {
		if _, exists := folders[folder.Name]; exists {
			continue
		}
		folders[folder.Name] = &ShareFolderStats{Name: folder.Name, Path: folder.Path}
		order = append(order, folder.Name)
	}
	for name := range s.byDirectory {
		if folder, ok := folders[shareFolderOf(name)]; ok {
			folder.Directories++
		}
	}

	s.search = make([]*indexedFile, 0, len(index.Files))
	for i := range index.Files {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		e := &index.Files[i]
		wire := peer.File{
			Name:      e.VirtualPath,
			Size:      uint64(e.Size),
			Extension: e.Extension,
			// attributesFromCache is reused so a stored zero/zero pair means
			// the same "examined, no attributes" it means in the metadata cache
			// rather than being advertised as a real 0 kbit/s.
			Attributes: attributesFromCache(ShareFileMeta{Bitrate: e.Bitrate, Duration: e.Duration}),
		}
		indexed := &indexedFile{
			virtual:      e.VirtualPath,
			virtualLower: strings.ToLower(e.VirtualPath),
			local:        e.LocalPath,
			root:         e.ShareRoot,
			wire:         wire,
			modTime:      e.ModTime,
		}
		s.files[indexed.virtual] = indexed
		s.search = append(s.search, indexed)

		if folder, ok := folders[shareFolderOf(e.VirtualPath)]; ok {
			folder.Files++
			folder.TotalBytes += wire.Size
		}

		dirVirtual := virtualDirectory(e.VirtualPath)
		directory := s.byDirectory[dirVirtual]
		directory.Name = dirVirtual
		fileInDirectory := wire
		fileInDirectory.Name = filepath.Base(e.LocalPath)
		directory.Files = append(directory.Files, fileInDirectory)
		s.byDirectory[dirVirtual] = directory
	}

	for _, name := range order {
		s.folders = append(s.folders, *folders[name])
	}
	for _, directory := range s.byDirectory {
		sort.Slice(directory.Files, func(i, j int) bool {
			return strings.ToLower(directory.Files[i].Name) < strings.ToLower(directory.Files[j].Name)
		})
		s.directories = append(s.directories, directory)
	}
	sort.Slice(s.directories, func(i, j int) bool {
		return strings.ToLower(s.directories[i].Name) < strings.ToLower(s.directories[j].Name)
	})
	sort.Slice(s.search, func(i, j int) bool {
		return s.search[i].virtualLower < s.search[j].virtualLower
	})

	trigrams, err := buildShareTrigramIndex(ctx, s.search)
	if err != nil {
		return nil, fmt.Errorf("build share search index: %w", err)
	}
	s.trigrams = trigrams

	var totalBytes uint64
	for _, folder := range s.folders {
		totalBytes += folder.TotalBytes
	}
	s.stats.Directories = len(s.directories)
	s.stats.Files = len(s.search)
	s.stats.TotalBytes = totalBytes

	frame, err := serializeSharedFileList(s.directories)
	if err != nil {
		if errors.Is(err, soul.ErrMessageTooLarge) {
			return nil, fmt.Errorf("%w: %d shared files in %d directories do not fit in a browse response, so sharing is disabled until fewer files are shared: %w",
				ErrShareTooLarge, len(s.search), len(s.directories), err)
		}
		return nil, fmt.Errorf("serialize shared file list: %w", err)
	}
	s.sharedFrame = frame
	return s, nil
}

// saveShareIndex persists a freshly published snapshot. It must be called
// inside the share-scan slot, after the snapshot is published: outside it, two
// saves could race over which one is "the latest scan", and the tables' whole
// invariant is that they hold exactly one.
//
// A failure is logged and swallowed. Sharing works — the index is in memory and
// peers are being served from it — so a scan is not marked failed over a
// database that could not take a copy; the only cost is that the next start
// scans the filesystem again.
func (c *Client) saveShareIndex(ctx context.Context, snapshot *shareSnapshot) {
	if c.cfg.ShareIndexStore == nil {
		return
	}
	index := &ShareIndex{
		ScannedAt:    snapshot.stats.IndexedAt,
		ScanDuration: snapshot.stats.ScanDuration,
		Folders:      append([]SharedFolder(nil), c.cfg.SharedFolders...),
		Directories:  make([]string, 0, len(snapshot.directories)),
		Files:        make([]ShareIndexEntry, 0, len(snapshot.search)),
		TotalBytes:   snapshot.stats.TotalBytes,
	}
	for _, directory := range snapshot.directories {
		index.Directories = append(index.Directories, directory.Name)
	}
	for _, indexed := range snapshot.search {
		bitrate, duration := attributeValues(indexed.wire.Attributes)
		index.Files = append(index.Files, ShareIndexEntry{
			VirtualPath: indexed.virtual,
			LocalPath:   indexed.local,
			ShareRoot:   indexed.root,
			Size:        int64(indexed.wire.Size),
			Extension:   indexed.wire.Extension,
			ModTime:     indexed.modTime,
			Bitrate:     bitrate,
			Duration:    duration,
		})
	}

	saveCtx, cancel := context.WithTimeout(ctx, c.cfg.shareIndexTimeout)
	defer cancel()
	if err := c.cfg.ShareIndexStore.SaveShareIndex(saveCtx, index); err != nil && c.logger != nil {
		c.logger.Warn("storing the share index failed; the next start will scan the filesystem", "err", err)
	}
}

// shareFolderOf returns the public shared-folder name a virtual path belongs
// to: its first backslash-separated segment, which scanShares builds from the
// configured name. A shared folder's own root directory is that segment alone.
func shareFolderOf(virtual string) string {
	if i := strings.IndexByte(virtual, '\\'); i >= 0 {
		return virtual[:i]
	}
	return virtual
}

// diffSharedFolders describes how the shared folders a stored index was
// scanned from differ from the ones configured now, or returns "" when they
// match. The description is the entire reason the folder set is stored in full
// rather than as a hash: a rejected index has to be able to say what changed.
//
// Order is not a difference — the configuration file's ordering does not affect
// what is indexed — but a folder's path is, since the same public name over a
// different directory offers entirely different files.
func diffSharedFolders(stored, configured []SharedFolder) string {
	storedByName := make(map[string]string, len(stored))
	for _, folder := range stored {
		storedByName[folder.Name] = folder.Path
	}
	configuredByName := make(map[string]string, len(configured))
	for _, folder := range configured {
		configuredByName[folder.Name] = folder.Path
	}

	var changes []string
	for _, folder := range configured {
		path, ok := storedByName[folder.Name]
		switch {
		case !ok:
			changes = append(changes, fmt.Sprintf("added %q", folder.Name))
		case path != folder.Path:
			changes = append(changes, fmt.Sprintf("%q moved from %q to %q", folder.Name, path, folder.Path))
		}
	}
	for _, folder := range stored {
		if _, ok := configuredByName[folder.Name]; !ok {
			changes = append(changes, fmt.Sprintf("removed %q", folder.Name))
		}
	}
	sort.Strings(changes)
	return strings.Join(changes, ", ")
}
