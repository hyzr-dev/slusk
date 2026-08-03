package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/distributed"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/server"
)

const (
	maxSharedSearchResults = 500
	// maxShareWorkers bounds the CPU-bound, no-new-connection share work: the
	// in-memory search match and folder-contents build (both send on an existing
	// session, if at all). It is generous because that work is microseconds and
	// opens no sockets, so nearly every inbound distributed search can be
	// evaluated instead of dropped before we even know whether we match.
	maxShareWorkers = 32
	// maxDeliverWorkers bounds the network-bound part: delivering a matched
	// search response, which opens (or reuses) a session to the searcher - for a
	// firewalled searcher an indirect connection consuming one of the shared
	// inbound leases. It is kept small so it leaves lease headroom for downloads;
	// dropping when full is acceptable backpressure.
	maxDeliverWorkers = 16
	searchResponseTTL = 2 * time.Minute
)

// ErrShareScanInProgress is returned by TriggerRescanShares when a share scan
// is already running; the caller should not queue a second one.
var ErrShareScanInProgress = errors.New("soulseek: share scan already in progress")

// ErrClientStopped is returned when the client is not running (or is
// shutting down) and cannot start background work.
var ErrClientStopped = errors.New("soulseek: client is not running")

// SharedFolder maps a private local directory to one explicitly named public
// virtual root. Local paths are never placed on the wire.
type SharedFolder struct {
	Name string
	Path string
}

// ShareStats is the published index size. It is a plain value type because
// it is stored by value in announcedStats and returned by value from
// RescanShares.
type ShareStats struct {
	Directories int
	Files       int
	// TotalBytes is the sum of every indexed file's advertised size. Content
	// that is hardlinked or otherwise duplicated on disk is counted once per
	// index entry, not once per inode - it reflects what is advertised to the
	// network, not deduplicated disk usage.
	TotalBytes uint64
	// IndexedAt is when this index finished scanning. Zero until the first
	// successful scan.
	IndexedAt time.Time
	// ScanDuration is how long the filesystem walk that produced this index
	// took, measured from after any test shareScanHook.
	ScanDuration time.Duration
}

// ShareFolderStats is one configured SharedFolder's contribution to the
// published index. Name and Path mirror the configured share (Path is the
// private local directory, never placed on the wire).
type ShareFolderStats struct {
	Name        string
	Path        string
	Directories int
	Files       int
	TotalBytes  uint64
}

// ShareReport is the full read-only view of the published share index: the
// aggregate stats, the per-folder breakdown, and whether a scan is currently
// running.
type ShareReport struct {
	ShareStats
	Folders  []ShareFolderStats
	Scanning bool
}

type indexedFile struct {
	virtual      string
	virtualLower string
	local        string
	root         string
	wire         peer.File
	info         os.FileInfo
}

type shareTrigram uint32

type shareSnapshot struct {
	stats       ShareStats
	folders     []ShareFolderStats
	files       map[string]*indexedFile
	search      []*indexedFile
	trigrams    map[shareTrigram][]uint32
	directories []peer.Directory
	byDirectory map[string]peer.Directory
	sharedFrame []byte
}

func emptyShareSnapshot() *shareSnapshot {
	msg := &peer.SharedFileListResponse{}
	frame, _ := msg.Serialize(msg)
	return &shareSnapshot{
		files:       map[string]*indexedFile{},
		trigrams:    map[shareTrigram][]uint32{},
		byDirectory: map[string]peer.Directory{},
		sharedFrame: frame,
	}
}

type searchDeliveryKey struct {
	username string
	token    soul.Token
}

// RescanShares builds a complete immutable index and publishes it atomically.
// If any configured root cannot be scanned, the prior snapshot remains live.
func (c *Client) RescanShares(ctx context.Context) (ShareStats, error) {
	stats, err := c.scanAndPublish(ctx)
	if err != nil {
		return ShareStats{}, err
	}
	if err := c.announceShares(); err != nil {
		return stats, fmt.Errorf("announce rescanned shares: %w", err)
	}
	return stats, nil
}

// scanAndPublish builds a complete immutable index and publishes it
// atomically to c.shares, without announcing it to the server. If any
// configured root cannot be scanned, the prior snapshot remains live. It
// blocks until the share-scan slot is free or ctx is done - unlike before
// issue #160, this can now return ctx.Err() while waiting for a concurrent
// scan, rather than blocking indefinitely; both callers (the SIGHUP path via
// RescanShares, and runInitialShareScan) already handle errors and pass a
// cancellable ctx. Callers that must not block behind a running scan should
// use TriggerRescanShares instead.
func (c *Client) scanAndPublish(ctx context.Context) (ShareStats, error) {
	if err := c.acquireShareScan(ctx); err != nil {
		return ShareStats{}, err
	}
	defer c.releaseShareScan()
	return c.scanAndPublishLocked(ctx)
}

// scanAndPublishLocked is scanAndPublish's body, run while already holding
// the share-scan slot. Callers must claim the slot themselves first, via
// acquireShareScan or tryAcquireShareScan.
func (c *Client) scanAndPublishLocked(ctx context.Context) (ShareStats, error) {
	snapshot, err := c.scanShares(ctx)
	if err != nil {
		return ShareStats{}, err
	}
	c.shares.Store(snapshot)

	if c.logger != nil {
		c.logger.Info("shares scanned",
			"directories", snapshot.stats.Directories,
			"files", snapshot.stats.Files,
			"bytes", snapshot.stats.TotalBytes,
			"duration", snapshot.stats.ScanDuration)
	}

	return snapshot.stats, nil
}

// TriggerRescanShares claims the share-scan slot and runs a rescan plus
// announcement in the background, returning as soon as the scan is claimed
// rather than waiting for the filesystem walk to finish. It never blocks on
// the scan itself, so an HTTP caller cannot be held open for the duration of
// a share rescan. Returns ErrShareScanInProgress if a scan is already
// running, or ErrClientStopped if the client's lifecycle is not active.
func (c *Client) TriggerRescanShares() error {
	if !c.tryAcquireShareScan() {
		return ErrShareScanInProgress
	}
	started := c.startTracked(func() {
		defer c.releaseShareScan()
		if _, err := c.scanAndPublishLocked(c.lifecycleContext()); err != nil {
			if c.logger != nil {
				c.logger.Error("background share rescan failed", "err", err)
			}
			return
		}
		// announceShares reads the currently published stats at send time and
		// skips the send when this server generation was already told
		// identical counts; a failed announce here is not fatal, since
		// serveConnected re-announces on every server login.
		if err := c.announceShares(); err != nil {
			if c.logger != nil {
				c.logger.Debug("share stats will be announced on next server login", "err", err)
			}
		}
	})
	if !started {
		// Release-on-failure is load-bearing: a leaked slot here would
		// deadlock every future blocking RescanShares/scanAndPublish call.
		c.releaseShareScan()
		return ErrClientStopped
	}
	return nil
}

// acquireShareScan claims the share-scan slot, blocking until it is free or
// ctx is done.
func (c *Client) acquireShareScan(ctx context.Context) error {
	// This first, non-blocking select only matters when the slot and
	// ctx.Done() are both ready at once, where a single select below would
	// pick between them at random; trying the slot alone first makes
	// acquiring it deterministically preferred over an already-cancelled ctx.
	select {
	case c.shareScanSem <- struct{}{}:
		return nil
	default:
	}
	select {
	case c.shareScanSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryAcquireShareScan claims the share-scan slot without blocking, reporting
// whether the claim succeeded.
func (c *Client) tryAcquireShareScan() bool {
	select {
	case c.shareScanSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseShareScan frees the share-scan slot claimed by acquireShareScan or
// tryAcquireShareScan. The semaphore channel trades away sync.Mutex's loud
// misuse detection (Unlock panics if not held), so this panics instead of
// silently blocking forever if the slot is not actually held.
func (c *Client) releaseShareScan() {
	select {
	case <-c.shareScanSem:
	default:
		panic("releaseShareScan: share scan slot not held")
	}
}

// ShareReport returns the currently published index's aggregate stats,
// per-folder breakdown, and whether a scan is running right now. It reads
// the published snapshot directly rather than the share-scan slot's owner,
// so it never blocks behind an in-progress scan; Scanning is therefore a UI
// hint that can race the read that follows it; TriggerRescanShares's 409 is
// the authoritative concurrency signal.
func (c *Client) ShareReport() ShareReport {
	s := c.shareSnapshot()
	folders := make([]ShareFolderStats, len(s.folders))
	copy(folders, s.folders)
	return ShareReport{
		ShareStats: s.stats,
		Folders:    folders,
		Scanning:   len(c.shareScanSem) > 0,
	}
}

// announceShares sends the currently published index size to the server, if
// a server connection is currently established. The snapshot-stats read and
// the send form one critical section under announceMu, shared by every
// announcer (serveConnected's login-time announcement, the initial background
// scan, SIGHUP rescans): whichever announcer sends later reads the later
// snapshot, so the wire order of announcements always matches publish order
// and the server always ends up on the latest published stats under any
// interleaving. announceMu also carries the dedup state: when the current
// server generation has already been told identical stats, the send is
// skipped (returns nil), so e.g. an initial scan that found no shares does
// not repeat the login-time 0/0 frame.
//
// Until the generation's login-time announcement has happened
// (announceSharesOnLogin), the send is also skipped: serveConnected's fixed
// post-login frame sequence must not be raced by a background scan that
// finishes first, and the pending login-time announcement is guaranteed to
// follow and reads the then-current stats at send time, so nothing published
// before it is ever lost. It is generation-guarded via
// sendToServerGeneration and a no-op (returns nil) when there is no active
// connection.
//
// The dedup comparison is on ShareStats.Directories/Files only - the counts
// that actually go on the wire - not the whole ShareStats value: IndexedAt
// and ScanDuration make every scan's stats value distinct, so comparing the
// full struct would defeat the dedup and re-send SharedFoldersFiles on every
// rescan even when the counts did not change.
func (c *Client) announceShares() error {
	return c.announceCurrentShares(false)
}

// announceSharesOnLogin is serveConnected's variant of announceShares: it
// performs the mandatory first announcement of a server generation (Soulseek
// expects an explicit index count on every authenticated session), which also
// unlocks announceShares sends for that generation.
func (c *Client) announceSharesOnLogin() error {
	return c.announceCurrentShares(true)
}

func (c *Client) announceCurrentShares(loginTime bool) error {
	c.announceMu.Lock()
	defer c.announceMu.Unlock()
	generation := c.currentServerGeneration()
	if generation == 0 {
		return nil
	}
	if !loginTime && c.announcedGeneration != generation {
		// The login-time announcement for this generation is still pending; it
		// will read the currently published stats when it sends.
		return nil
	}
	stats := c.shareSnapshot().stats
	if c.announcedGeneration == generation &&
		c.announcedStats.Directories == stats.Directories &&
		c.announcedStats.Files == stats.Files {
		return nil
	}
	if err := sendToServerGeneration(c, generation, &server.SharedFoldersFiles{
		Directories: stats.Directories, Files: stats.Files,
	}); err != nil {
		return err
	}
	c.announcedGeneration = generation
	c.announcedStats = stats
	return nil
}

// runInitialShareScan builds and publishes the initial share index in the
// background, so Run can connect to and log in with the server without
// waiting on a scan of the local filesystem (a boot-time disk blip or
// not-yet-ready mount can otherwise take arbitrarily long). Until this
// completes, the client answers browse/search requests with the empty
// snapshot New installs. Retries forever with the same exponential backoff
// retryStartup uses, since there is no clean terminal classification for a
// scan failure and a genuinely bad config is caught by config.Validate first.
// It never returns early on ctx cancellation mid-attempt beyond the standard
// per-attempt check, since scanAndPublish/scanShares already thread ctx
// through the filesystem walk.
func (c *Client) runInitialShareScan(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		stats, err := c.scanAndPublish(ctx)
		if err == nil {
			c.logger.Info("initial share scan complete",
				"directories", stats.Directories, "files", stats.Files)
			// announceShares reads the currently published stats at send time
			// and skips the send when this server generation was already told
			// identical stats (most commonly: no shares configured, so the
			// login-time announcement already carried the empty 0/0 counts).
			// serveConnected re-announces current share stats on every server
			// login (generation-guarded), so a failed announce here is not
			// fatal: it will simply be picked up on the next login instead of
			// forcing a rescan.
			if announceErr := c.announceShares(); announceErr != nil {
				c.logger.Debug("share stats will be announced on next server login", "err", announceErr)
			}
			return
		}
		wait := nextBackoff(attempt, c.cfg.backoffBase, c.cfg.backoffCap)
		c.logger.Warn("initial share scan failed; retrying in background",
			"err", err, "backoff", wait, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *Client) scanShares(ctx context.Context) (*shareSnapshot, error) {
	if c.cfg.shareScanHook != nil {
		if err := c.cfg.shareScanHook(ctx); err != nil {
			return nil, err
		}
	}
	s := &shareSnapshot{files: make(map[string]*indexedFile), byDirectory: make(map[string]peer.Directory)}
	names := make(map[string]struct{}, len(c.cfg.SharedFolders))
	paths := make(map[string]struct{}, len(c.cfg.SharedFolders))
	start := time.Now()
	lastLog := start

	cached, cacheActive := c.loadShareMetaCache(ctx)
	observed := make(map[string]ShareFileMeta, len(cached))
	var pending []ShareFileMeta
	for _, configured := range c.cfg.SharedFolders {
		name := strings.TrimSpace(configured.Name)
		if name == "" || name != configured.Name || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
			return nil, fmt.Errorf("invalid public share name %q", configured.Name)
		}
		if _, exists := names[strings.ToLower(name)]; exists {
			return nil, fmt.Errorf("duplicate public share name %q", configured.Name)
		}
		names[strings.ToLower(name)] = struct{}{}
		if strings.TrimSpace(configured.Path) != configured.Path || !filepath.IsAbs(configured.Path) {
			return nil, fmt.Errorf("share %q path must be absolute and contain no surrounding whitespace", configured.Name)
		}
		cleanPath := filepath.Clean(configured.Path)
		if _, exists := paths[cleanPath]; exists {
			return nil, fmt.Errorf("duplicate share path %q", configured.Path)
		}
		paths[cleanPath] = struct{}{}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root, err := filepath.EvalSymlinks(configured.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve share %q: %w", configured.Name, err)
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve absolute share %q: %w", configured.Name, err)
		}
		root = filepath.Clean(root)
		rootInfo, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("stat share %q: %w", configured.Name, err)
		}
		if !rootInfo.IsDir() {
			return nil, fmt.Errorf("share %q path is not a directory", configured.Name)
		}

		folder := ShareFolderStats{Name: configured.Name, Path: configured.Path}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if path != root && errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path != root && strings.ContainsRune(entry.Name(), '\\') {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if path != root && entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if c.logger != nil && time.Since(lastLog) >= c.cfg.shareScanLogInterval {
				lastLog = time.Now()
				c.logger.Info("share scan in progress",
					"share", configured.Name,
					"directories", len(s.byDirectory),
					"files", len(s.files),
					"elapsed", time.Since(start))
			}
			rel, err := filepath.Rel(root, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("share %q path escaped root", configured.Name)
			}
			virtual := configured.Name
			if rel != "." {
				virtual += `\` + strings.ReplaceAll(rel, string(filepath.Separator), `\`)
			}
			if entry.IsDir() {
				if _, exists := s.byDirectory[virtual]; !exists {
					s.byDirectory[virtual] = peer.Directory{Name: virtual}
					folder.Directories++
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			resolvedFile, err := filepath.EvalSymlinks(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			resolvedFile, err = filepath.Abs(resolvedFile)
			if err != nil || !pathWithinRoot(root, resolvedFile) {
				return fmt.Errorf("share %q file escaped resolved root: %s", configured.Name, path)
			}
			resolvedInfo, err := os.Stat(resolvedFile)
			if err != nil || !resolvedInfo.Mode().IsRegular() || !os.SameFile(info, resolvedInfo) {
				return fmt.Errorf("share %q file changed while resolving: %s", configured.Name, path)
			}
			dirVirtual := virtualDirectory(virtual)
			wire := peer.File{Name: virtual, Size: uint64(info.Size()), Extension: extensionOf(filepath.Base(path))}
			if audioFormatOf(path) == "" {
				// Not an audio format: extractTechnicalMetadata returns nil without
				// opening anything, so a cache row would be pure table growth.
			} else if hit, ok := cached[path]; ok && sameShareFileMeta(hit, info.Size(), info.ModTime()) {
				wire.Attributes = attributesFromCache(hit)
				observed[path] = hit
			} else {
				wire.Attributes = extractTechnicalMetadata(path, info.Size(), c.logger)
				bitrate, duration := attributeValues(wire.Attributes)
				entry := ShareFileMeta{Path: path, Size: info.Size(), ModTime: info.ModTime(),
					Bitrate: bitrate, Duration: duration}
				observed[path] = entry
				pending = append(pending, entry)
			}
			resolvedAfter, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("re-resolve share %q file: %w", configured.Name, err)
			}
			resolvedAfter, err = filepath.Abs(resolvedAfter)
			if err != nil || !pathWithinRoot(root, resolvedAfter) {
				return fmt.Errorf("share %q file escaped resolved root during scan: %s", configured.Name, path)
			}
			afterInfo, err := os.Stat(resolvedAfter)
			if err != nil || !afterInfo.Mode().IsRegular() || !os.SameFile(info, afterInfo) {
				return fmt.Errorf("share %q file changed during scan: %s", configured.Name, path)
			}
			indexed := &indexedFile{
				virtual:      virtual,
				virtualLower: strings.ToLower(virtual),
				local:        path,
				root:         root,
				wire:         wire,
				info:         info,
			}
			s.files[virtual] = indexed
			s.search = append(s.search, indexed)
			folder.Files++
			folder.TotalBytes += wire.Size
			directory := s.byDirectory[dirVirtual]
			directory.Name = dirVirtual
			fileInDirectory := wire
			fileInDirectory.Name = filepath.Base(path)
			directory.Files = append(directory.Files, fileInDirectory)
			s.byDirectory[dirVirtual] = directory
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan share %q: %w", configured.Name, err)
		}
		s.folders = append(s.folders, folder)
	}

	// Only reached once every configured root has walked successfully, so a
	// failed scan never prunes cache rows for a root it did not finish -
	// they would otherwise be reloaded and re-verified on the next attempt.
	c.flushShareMetaCache(ctx, cached, observed, pending, cacheActive, len(s.files))

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
	s.stats = ShareStats{
		Directories:  len(s.directories),
		Files:        len(s.search),
		TotalBytes:   totalBytes,
		IndexedAt:    time.Now(),
		ScanDuration: time.Since(start),
	}
	msg := &peer.SharedFileListResponse{Directories: s.directories}
	frame, err := msg.Serialize(msg)
	if err != nil {
		return nil, fmt.Errorf("serialize shared file list: %w", err)
	}
	s.sharedFrame = frame
	return s, nil
}

// loadShareMetaCache loads every cached row up front, before the scan starts
// touching the filesystem, and reports whether the cache is active
// (c.cfg.ShareMetaCache != nil AND the load succeeded). On any failure - no
// cache configured, or a load error - it returns (nil, false); scanShares
// then reads every audio file exactly as it did before this cache existed,
// and flushShareMetaCache below skips writing back.
func (c *Client) loadShareMetaCache(ctx context.Context) (map[string]ShareFileMeta, bool) {
	if c.cfg.ShareMetaCache == nil {
		return nil, false
	}
	loadCtx, cancel := context.WithTimeout(ctx, c.cfg.shareMetaCacheTimeout)
	defer cancel()
	entries, err := c.cfg.ShareMetaCache.LoadShareMeta(loadCtx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("load share metadata cache failed; reading every shared audio file this scan", "err", err)
		}
		return nil, false
	}
	cached := make(map[string]ShareFileMeta, len(entries))
	for _, e := range entries {
		cached[e.Path] = e
	}
	if c.logger != nil {
		c.logger.Debug("share metadata cache loaded", "rows", len(cached))
	}
	return cached, true
}

// flushShareMetaCache writes back this scan's cache results: pending is every
// entry freshly computed this scan (a miss or a stale hit), and stale is
// every path in cached that this scan did not observe - which is deleted so
// the cache never grows unboundedly stale as files are removed or renamed.
// It is a no-op if the cache was not active for this scan, and never returns
// an error: a save failure only costs the next scan a re-read, never
// correctness.
//
// totalFiles is the count of every file the walk actually indexed this scan
// (audio and non-audio alike, i.e. len(shareSnapshot.files)), not len(observed)
// - observed only ever gains entries for audio files, so a share containing
// files but no mp3/flac would otherwise look indistinguishable from an empty
// mount and permanently disable pruning. If totalFiles is zero while cached
// held rows, the scan almost certainly saw a share root that is present but
// (transiently) empty - e.g. a mount that dropped mid-walk without erroring.
// Pruning in that case would delete the entire cache for one bad tick, so it
// is skipped with a warning instead.
func (c *Client) flushShareMetaCache(ctx context.Context, cached, observed map[string]ShareFileMeta, pending []ShareFileMeta, cacheActive bool, totalFiles int) {
	if !cacheActive {
		return
	}
	var stale []string
	if totalFiles == 0 && len(cached) > 0 {
		if c.logger != nil {
			c.logger.Warn("share metadata cache: scan observed zero files while the cache held rows; skipping prune",
				"cached_rows", len(cached))
		}
	} else {
		for path := range cached {
			if _, ok := observed[path]; !ok {
				stale = append(stale, path)
			}
		}
	}

	saveCtx, cancel := context.WithTimeout(ctx, c.cfg.shareMetaCacheTimeout)
	defer cancel()
	if err := c.cfg.ShareMetaCache.SaveShareMeta(saveCtx, pending, stale); err != nil && c.logger != nil {
		c.logger.Warn("save share metadata cache failed", "err", err)
	}
}

func virtualDirectory(name string) string {
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		return name[:i]
	}
	return ""
}

func extensionOf(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return ext
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func normalizeVirtualPath(value string) (string, bool) {
	value = strings.ReplaceAll(value, "/", `\`)
	if value == "" || strings.HasPrefix(value, `\`) || strings.HasSuffix(value, `\`) {
		return "", false
	}
	parts := strings.Split(value, `\`)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return strings.Join(parts, `\`), true
}

func (c *Client) shareSnapshot() *shareSnapshot {
	if s := c.shares.Load(); s != nil {
		return s
	}
	return emptyShareSnapshot()
}

func (s *shareSnapshot) folderResponse(token soul.Token, requested string) *peer.FolderContentsResponse {
	response := &peer.FolderContentsResponse{Token: token, Folder: requested}
	normalized, ok := normalizeVirtualPath(requested)
	if !ok {
		return response
	}
	prefix := normalized + `\`
	// s.directories is sorted case-insensitively (see scanShares), so every directory
	// whose name matches or is nested under normalized (case-sensitively) falls within
	// the case-insensitive range [normalized, normalized+"]"). ']' (0x5D) is the first
	// byte above '\' (0x5C) and is unaffected by ToLower, so the upper bound is exact:
	// any lowered name >= lowered+"]" cannot equal normalized or start with prefix.
	lowered := strings.ToLower(normalized)
	lo := sort.Search(len(s.directories), func(i int) bool {
		return strings.ToLower(s.directories[i].Name) >= lowered
	})
	hi := sort.Search(len(s.directories), func(i int) bool {
		return strings.ToLower(s.directories[i].Name) >= lowered+"]"
	})
	for _, directory := range s.directories[lo:hi] {
		if directory.Name == normalized || strings.HasPrefix(directory.Name, prefix) {
			response.Folders = append(response.Folders, directory)
		}
	}
	return response
}

type sharingSessionHooks struct{ c *Client }

func (*sharingSessionHooks) established(*peerSession)   {}
func (*sharingSessionHooks) closed(*peerSession, error) {}

func (h *sharingSessionHooks) frame(session *peerSession, frame sessionFrame) error {
	if frame.connType != peer.ConnectionType {
		return errUnhandledPeerFrame
	}
	switch peer.Code(frame.code) {
	case peer.CodeSharedFileListRequest:
		request := &peer.SharedFileListRequest{}
		if err := request.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize shared file list request: %w", err)
		}
		if !session.TrySend(h.c.shareSnapshot().sharedFrame) && h.c.logger != nil {
			h.c.logger.Debug("dropping shared file list response due to peer backpressure", "username", session.key.username)
		}
		return nil
	case peer.CodeFolderContentsRequest:
		request := &peer.FolderContentsRequest{}
		if err := request.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize folder contents request: %w", err)
		}
		select {
		case h.c.shareWorkers <- struct{}{}:
		default:
			if h.c.logger != nil {
				h.c.logger.Debug("dropping folder contents response due to worker saturation", "username", session.key.username)
			}
			return nil
		}
		if !h.c.startTracked(func() {
			defer func() { <-h.c.shareWorkers }()
			// The serialized frame cannot be precomputed/cached per folder: the request
			// token is embedded inside the zlib-compressed body (see
			// soul/peer/foldercontentsresponse.go Serialize), so it must be built fresh
			// for every request.
			msg := h.c.shareSnapshot().folderResponse(request.Token, request.Folder)
			response, err := msg.Serialize(msg)
			if err != nil || !session.TrySend(response) {
				if h.c.logger != nil {
					h.c.logger.Debug("dropping folder contents response", "username", session.key.username, "err", err)
				}
			}
		}) {
			<-h.c.shareWorkers
		}
		return nil
	default:
		return errUnhandledPeerFrame
	}
}

func (c *Client) respondToSearch(username string, token soul.Token, query string) {
	if username == "" || username == c.cfg.Username {
		return
	}
	// Match slot: cheap, connectionless work, freed the instant the match is done
	// so a slow delivery below can never pin it (that is a separate pool).
	select {
	case c.shareWorkers <- struct{}{}:
	case <-c.lifecycleContext().Done():
		return
	default:
		if c.logger != nil {
			c.logger.Debug("dropping share search due to match worker saturation", "username", username)
		}
		return
	}
	if !c.startTracked(func() {
		// Release the match slot as soon as the match returns (deferred so a
		// panic in match cannot leak the slot), before any network I/O.
		results := func() []peer.File {
			defer func() { <-c.shareWorkers }()
			return c.shareSnapshot().match(query, maxSharedSearchResults)
		}()
		if len(results) == 0 || !c.reserveSearchDelivery(username, token) {
			return
		}
		delivered := false
		defer func() { c.finishSearchDelivery(username, token, delivered) }()

		// Delivery slot: separate, smaller pool. Opening a session to the searcher
		// (indirect for a firewalled one) consumes a shared inbound lease, so drop
		// rather than queue when this network-bound pool is full.
		select {
		case c.deliverWorkers <- struct{}{}:
		default:
			if c.logger != nil {
				c.logger.Debug("dropping share search response due to deliver worker saturation", "username", username)
			}
			return
		}
		defer func() { <-c.deliverWorkers }()

		ctx, cancel := context.WithTimeout(c.lifecycleContext(), c.cfg.establishTimeout)
		defer cancel()
		session, err := c.getOrConnectPeerSession(ctx, username)
		if err != nil {
			return
		}
		free, queued := c.uploads.availability()
		msg := &peer.FileSearchResponse{Username: c.cfg.Username, Token: token, Results: results, FreeSlot: free, Queue: queued}
		wire, err := msg.Serialize(msg)
		if err == nil {
			delivered = session.TrySend(wire)
		}
		if !delivered && c.logger != nil {
			c.logger.Debug("dropping share search response", "username", username, "err", err)
		}
	}) {
		<-c.shareWorkers
	}
}

func packShareTrigram(value string, offset int) shareTrigram {
	return shareTrigram(uint32(value[offset])<<16 | uint32(value[offset+1])<<8 | uint32(value[offset+2]))
}

func buildShareTrigramIndex(ctx context.Context, search []*indexedFile) (map[shareTrigram][]uint32, error) {
	if uint64(len(search)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("too many shared files: %d", len(search))
	}
	type buildState struct {
		start uint64
		count uint32
		next  uint32
		last  uint32
	}
	states := make(map[shareTrigram]buildState)
	for position, indexed := range search {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		marker := uint32(position) + 1
		path := indexed.virtualLower
		for offset := 0; offset+3 <= len(path); offset++ {
			if offset&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			gram := packShareTrigram(path, offset)
			state := states[gram]
			if state.last != marker {
				if state.count == ^uint32(0) {
					return nil, fmt.Errorf("too many postings for trigram %06x", uint32(gram))
				}
				state.count++
				state.last = marker
				states[gram] = state
			}
		}
	}

	var postingCount uint64
	stateNumber := 0
	for gram, state := range states {
		if stateNumber&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		stateNumber++
		state.start = postingCount
		state.last = 0
		states[gram] = state
		postingCount += uint64(state.count)
	}
	maxInt := uint64(^uint(0) >> 1)
	if postingCount > maxInt {
		return nil, fmt.Errorf("share search index has too many postings: %d", postingCount)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload := make([]uint32, int(postingCount))
	for position, indexed := range search {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := uint32(position)
		marker := id + 1
		path := indexed.virtualLower
		for offset := 0; offset+3 <= len(path); offset++ {
			if offset&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			gram := packShareTrigram(path, offset)
			state := states[gram]
			if state.last == marker {
				continue
			}
			payload[int(state.start)+int(state.next)] = id
			state.next++
			state.last = marker
			states[gram] = state
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index := make(map[shareTrigram][]uint32, len(states))
	stateNumber = 0
	for gram, state := range states {
		if stateNumber&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		stateNumber++
		start := int(state.start)
		index[gram] = payload[start : start+int(state.count) : start+int(state.count)]
	}
	return index, nil
}

func parseShareSearchQuery(query string) (include, exclude []string) {
	includeSeen := make(map[string]struct{})
	excludeSeen := make(map[string]struct{})
	for _, term := range strings.Fields(strings.ToLower(strings.ReplaceAll(query, "/", `\`))) {
		if strings.HasPrefix(term, "-") && len(term) > 1 {
			term = term[1:]
			if _, exists := excludeSeen[term]; !exists {
				excludeSeen[term] = struct{}{}
				exclude = append(exclude, term)
			}
		} else if term != "" {
			if _, exists := includeSeen[term]; !exists {
				includeSeen[term] = struct{}{}
				include = append(include, term)
			}
		}
	}
	return include, exclude
}

func matchesShareSearch(indexed *indexedFile, include, exclude []string) bool {
	for _, term := range include {
		if !strings.Contains(indexed.virtualLower, term) {
			return false
		}
	}
	for _, term := range exclude {
		if strings.Contains(indexed.virtualLower, term) {
			return false
		}
	}
	return true
}

func (s *shareSnapshot) match(query string, limit int) []peer.File {
	if limit <= 0 {
		return nil
	}
	include, exclude := parseShareSearchQuery(query)
	if len(include) == 0 {
		return nil
	}

	seen := make(map[shareTrigram]struct{})
	var postings [][]uint32
	for _, term := range include {
		var rarest []uint32
		var rarestGram shareTrigram
		for offset := 0; offset+3 <= len(term); offset++ {
			gram := packShareTrigram(term, offset)
			posting := s.trigrams[gram]
			if len(posting) == 0 {
				return nil
			}
			if rarest == nil || len(posting) < len(rarest) {
				rarest = posting
				rarestGram = gram
			}
		}
		if rarest != nil {
			if _, exists := seen[rarestGram]; !exists {
				seen[rarestGram] = struct{}{}
				postings = append(postings, rarest)
			}
		}
	}

	results := make([]peer.File, 0, min(limit, len(s.search)))
	if len(postings) == 0 {
		for _, indexed := range s.search {
			if matchesShareSearch(indexed, include, exclude) {
				results = append(results, indexed.wire)
				if len(results) == limit {
					break
				}
			}
		}
		return results
	}

	sort.Slice(postings, func(i, j int) bool { return len(postings[i]) < len(postings[j]) })
	cursors := make([]int, len(postings)-1)
	for _, id := range postings[0] {
		candidate := true
		for postingIndex := 1; postingIndex < len(postings); postingIndex++ {
			posting := postings[postingIndex]
			cursor := &cursors[postingIndex-1]
			for *cursor < len(posting) && posting[*cursor] < id {
				*cursor++
			}
			if *cursor == len(posting) || posting[*cursor] != id {
				candidate = false
				break
			}
		}
		if candidate && int(id) < len(s.search) {
			indexed := s.search[id]
			if matchesShareSearch(indexed, include, exclude) {
				results = append(results, indexed.wire)
				if len(results) == limit {
					break
				}
			}
		}
	}
	return results
}

func (c *Client) reserveSearchDelivery(username string, token soul.Token) bool {
	now := time.Now()
	key := searchDeliveryKey{username: username, token: token}
	c.searchDeliveryMu.Lock()
	defer c.searchDeliveryMu.Unlock()
	for old, expiry := range c.searchDeliveries {
		if !now.Before(expiry) {
			delete(c.searchDeliveries, old)
		}
	}
	if _, exists := c.searchDeliveryInFlight[key]; exists {
		return false
	}
	if expiry, exists := c.searchDeliveries[key]; exists && now.Before(expiry) {
		return false
	}
	// Hard cap protects memory even if tokens/requesters are deliberately
	// unique. Never evict an in-flight reservation, since doing so could admit
	// a concurrent duplicate; evict a completed TTL entry or reject instead.
	if len(c.searchDeliveries)+len(c.searchDeliveryInFlight) >= 4096 {
		for old := range c.searchDeliveries {
			delete(c.searchDeliveries, old)
			break
		}
		if len(c.searchDeliveries)+len(c.searchDeliveryInFlight) >= 4096 {
			return false
		}
	}
	c.searchDeliveryInFlight[key] = struct{}{}
	return true
}

func (c *Client) finishSearchDelivery(username string, token soul.Token, delivered bool) {
	key := searchDeliveryKey{username: username, token: token}
	c.searchDeliveryMu.Lock()
	delete(c.searchDeliveryInFlight, key)
	if delivered {
		c.searchDeliveries[key] = time.Now().Add(searchResponseTTL)
	}
	c.searchDeliveryMu.Unlock()
}

func (c *Client) handleDistributedShareSearch(search distributed.Search, _ []byte) {
	c.respondToSearch(search.Username, search.Token, search.Query)
}
