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

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

const (
	maxSharedSearchResults = 500
	maxShareWorkers        = 4
	searchResponseTTL      = 2 * time.Minute
)

// SharedFolder maps a private local directory to one explicitly named public
// virtual root. Local paths are never placed on the wire.
type SharedFolder struct {
	Name string
	Path string
}

// ShareStats is the published index size.
type ShareStats struct {
	Directories int
	Files       int
}

type indexedFile struct {
	virtual string
	local   string
	wire    peer.File
	info    os.FileInfo
}

type shareSnapshot struct {
	stats       ShareStats
	files       map[string]*indexedFile
	search      []*indexedFile
	directories []peer.Directory
	byDirectory map[string]peer.Directory
	sharedFrame []byte
}

func emptyShareSnapshot() *shareSnapshot {
	msg := &peer.SharedFileListResponse{}
	frame, _ := msg.Serialize(msg)
	return &shareSnapshot{files: map[string]*indexedFile{}, byDirectory: map[string]peer.Directory{}, sharedFrame: frame}
}

type searchDeliveryKey struct {
	username string
	token    soul.Token
}

// RescanShares builds a complete immutable index and publishes it atomically.
// If any configured root cannot be scanned, the prior snapshot remains live.
func (c *Client) RescanShares(ctx context.Context) (ShareStats, error) {
	c.shareScanMu.Lock()
	defer c.shareScanMu.Unlock()

	snapshot, err := c.scanShares(ctx)
	if err != nil {
		return ShareStats{}, err
	}
	c.shares.Store(snapshot)

	if generation := c.currentServerGeneration(); generation != 0 {
		if err := sendToServerGeneration(c, generation, &server.SharedFoldersFiles{
			Directories: snapshot.stats.Directories, Files: snapshot.stats.Files,
		}); err != nil {
			return snapshot.stats, fmt.Errorf("announce rescanned shares: %w", err)
		}
	}
	return snapshot.stats, nil
}

func (c *Client) scanShares(ctx context.Context) (*shareSnapshot, error) {
	s := &shareSnapshot{files: make(map[string]*indexedFile), byDirectory: make(map[string]peer.Directory)}
	names := make(map[string]struct{}, len(c.cfg.SharedFolders))
	paths := make(map[string]struct{}, len(c.cfg.SharedFolders))
	for _, configured := range c.cfg.SharedFolders {
		name := strings.TrimSpace(configured.Name)
		if name == "" || name != configured.Name || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
			return nil, fmt.Errorf("invalid public share name %q", configured.Name)
		}
		if _, exists := names[strings.ToLower(name)]; exists {
			return nil, fmt.Errorf("duplicate public share name %q", configured.Name)
		}
		names[strings.ToLower(name)] = struct{}{}
		if !filepath.IsAbs(configured.Path) {
			return nil, fmt.Errorf("share %q path must be absolute", configured.Name)
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
		rootInfo, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("stat share %q: %w", configured.Name, err)
		}
		if !rootInfo.IsDir() {
			return nil, fmt.Errorf("share %q path is not a directory", configured.Name)
		}

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
			if path != root && entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
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
			dirVirtual := virtualDirectory(virtual)
			wire := peer.File{Name: virtual, Size: uint64(info.Size()), Extension: extensionOf(virtual)}
			wire.Attributes = extractTechnicalMetadata(path, info.Size(), c.logger)
			indexed := &indexedFile{virtual: virtual, local: path, wire: wire, info: info}
			s.files[virtual] = indexed
			s.search = append(s.search, indexed)
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
		return strings.ToLower(s.search[i].virtual) < strings.ToLower(s.search[j].virtual)
	})
	s.stats = ShareStats{Directories: len(s.directories), Files: len(s.search)}
	msg := &peer.SharedFileListResponse{Directories: s.directories}
	frame, err := msg.Serialize(msg)
	if err != nil {
		return nil, fmt.Errorf("serialize shared file list: %w", err)
	}
	s.sharedFrame = frame
	return s, nil
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
	for _, directory := range s.directories {
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
		if !session.TrySend(h.c.shareSnapshot().sharedFrame) {
			return errors.New("share response backpressure")
		}
		return nil
	case peer.CodeFolderContentsRequest:
		request := &peer.FolderContentsRequest{}
		if err := request.Deserialize(bytes.NewReader(frame.wire)); err != nil {
			return fmt.Errorf("deserialize folder contents request: %w", err)
		}
		msg := h.c.shareSnapshot().folderResponse(request.Token, request.Folder)
		select {
		case h.c.shareWorkers <- struct{}{}:
		default:
			return errors.New("share response worker capacity exceeded")
		}
		if !h.c.startTracked(func() {
			defer func() { <-h.c.shareWorkers }()
			response, err := msg.Serialize(msg)
			if err != nil || !session.TrySend(response) {
				session.Close(errors.New("folder contents response failed"))
			}
		}) {
			<-h.c.shareWorkers
			return errors.New("share response lifecycle stopping")
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
	snapshot := c.shareSnapshot()
	results := snapshot.match(query, maxSharedSearchResults)
	if len(results) == 0 {
		return
	}
	select {
	case c.shareWorkers <- struct{}{}:
	case <-c.lifecycleContext().Done():
		return
	default:
		return
	}
	if !c.markSearchDelivery(username, token) {
		<-c.shareWorkers
		return
	}
	if !c.startTracked(func() {
		defer func() { <-c.shareWorkers }()
		ctx, cancel := context.WithTimeout(c.lifecycleContext(), c.cfg.establishTimeout)
		defer cancel()
		session, err := c.getOrConnectPeerSession(ctx, username)
		if err != nil {
			return
		}
		free, queued := c.uploads.availability()
		msg := &peer.FileSearchResponse{Username: c.cfg.Username, Token: token, Results: results, FreeSlot: free, Queue: queued}
		wire, err := msg.Serialize(msg)
		if err != nil || !session.TrySend(wire) {
			session.Close(errors.New("search response backpressure"))
		}
	}) {
		<-c.shareWorkers
		return
	}
}

func (s *shareSnapshot) match(query string, limit int) []peer.File {
	var include, exclude []string
	for _, term := range strings.Fields(strings.ToLower(strings.ReplaceAll(query, "/", `\`))) {
		if strings.HasPrefix(term, "-") && len(term) > 1 {
			exclude = append(exclude, term[1:])
		} else if term != "" {
			include = append(include, term)
		}
	}
	if len(include) == 0 {
		return nil
	}
	results := make([]peer.File, 0, min(limit, len(s.search)))
	for _, indexed := range s.search {
		path := strings.ToLower(indexed.virtual)
		matched := true
		for _, term := range include {
			if !strings.Contains(path, term) {
				matched = false
				break
			}
		}
		for _, term := range exclude {
			if strings.Contains(path, term) {
				matched = false
				break
			}
		}
		if matched {
			results = append(results, indexed.wire)
			if len(results) == limit {
				break
			}
		}
	}
	return results
}

func (c *Client) markSearchDelivery(username string, token soul.Token) bool {
	now := time.Now()
	key := searchDeliveryKey{username: username, token: token}
	c.searchDeliveryMu.Lock()
	defer c.searchDeliveryMu.Unlock()
	for old, expiry := range c.searchDeliveries {
		if now.After(expiry) {
			delete(c.searchDeliveries, old)
		}
	}
	if expiry, exists := c.searchDeliveries[key]; exists && now.Before(expiry) {
		return false
	}
	// Hard cap protects memory even if tokens/requesters are deliberately unique.
	if len(c.searchDeliveries) >= 4096 {
		for old := range c.searchDeliveries {
			delete(c.searchDeliveries, old)
			break
		}
	}
	c.searchDeliveries[key] = now.Add(searchResponseTTL)
	return true
}

func (c *Client) handleDistributedShareSearch(search distributed.Search, _ []byte) {
	c.respondToSearch(search.Username, search.Token, search.Query)
}
