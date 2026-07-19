package pipeline

// This file declares the narrow port interfaces pipeline modules consume (Go
// style: the consumer declares the interface), so the concrete slskd/store
// types satisfy them implicitly.

import (
	"context"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// MusicSource is the slice of the Lidarr client the discoverer needs.
type MusicSource interface {
	WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error)
	ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error)
	ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error
	AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error)
	AlbumReleases(ctx context.Context, albumID int64) ([]lidarr.AlbumRelease, error)
}

// PeerSearcher is the slice of the slskd client the discoverer needs for search+enqueue.
type PeerSearcher interface {
	Search(ctx context.Context, query string, timeout time.Duration) ([]core.SearchResult, error)
	Enqueue(ctx context.Context, username, filename string, size int64) (string, error)
	// Cancel stops a still-active slskd download, so a failed attempt's live
	// sibling transfers stop writing into a folder that is about to be cleaned
	// up. It cancels the in-flight transfer but leaves the terminal record in
	// slskd (use Remove to purge that). Same call the reconciler uses for
	// deadline-overdue transfers.
	Cancel(ctx context.Context, username, id string) error
	DeleteDownloadFolder(ctx context.Context, name string) error
}

// PeerNetwork is the slice of the slskd client the engine needs.
type PeerNetwork interface {
	ListDownloads(ctx context.Context) ([]slskd.Transfer, error)
	Cancel(ctx context.Context, username, id string) error
	// Remove purges a terminal transfer's leftover record from slskd (DELETE
	// ?remove=true). reconcile calls it immediately after the store marks a
	// transfer terminal, so slskd's transfer list does not accumulate every
	// finished download. It MUST run only after that store write — otherwise the
	// next reconcile pass would see the transfer gone from slskd's live list and
	// mis-handle it as "lost".
	Remove(ctx context.Context, username, id string) error
}

// Ranker ranks search results into candidates (satisfied by matcher.Scorer).
type Ranker interface {
	Rank(results []core.SearchResult, rel map[string]core.PeerReliability, now time.Time) []core.RankedCandidate
}
