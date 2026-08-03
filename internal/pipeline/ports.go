package pipeline

// This file declares the narrow port interfaces pipeline modules consume (Go
// style: the consumer declares the interface), so the concrete slskd/store
// types satisfy them implicitly.

import (
	"context"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// MusicSource is the slice of the Lidarr client the discoverer needs.
type MusicSource interface {
	WantedMissing(ctx context.Context) ([]core.WantedRelease, error)
	ManualImportCandidates(ctx context.Context, folder string) ([]core.ImportItem, error)
	ExecuteManualImport(ctx context.Context, items []core.ImportItem) error
	AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error)
	AlbumReleases(ctx context.Context, albumID int64) ([]core.AlbumRelease, error)
	// AlbumTracks is used by the discovery relevance gate (#316) to check
	// candidate filenames against the album's real tracklist. A failure
	// degrades that gate to a directory-only check rather than aborting
	// discovery - see discovery.go's searchJob for why.
	AlbumTracks(ctx context.Context, albumID int64) ([]core.AlbumTrack, error)
	// AlbumByForeignID resolves a manual job's AlbumMBID to a real Lidarr
	// album id (issue #59), used by Importing's verify phase before it can
	// call AlbumStatus for a manual job. found is false when the release
	// group is not in Lidarr's library, distinct from a transient error.
	AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
}

// PeerSearcher is the slice of the peer backend (slskd daemon or native
// soulseek client) the discoverer needs for search+enqueue.
type PeerSearcher interface {
	Search(ctx context.Context, query string, timeout time.Duration) ([]core.SearchResult, error)
	Enqueue(ctx context.Context, username, filename string, size int64) (string, error)
	// Cancel stops a still-active download, so a failed attempt's live
	// sibling transfers stop writing into a folder that is about to be cleaned
	// up. It cancels the in-flight transfer but leaves the terminal record in
	// the backend (use Remove to purge that). Same call the reconciler uses
	// for deadline-overdue transfers.
	Cancel(ctx context.Context, username, id string) error
	DeleteDownloadFolder(ctx context.Context, name string) error
}

// PeerNetwork is the slice of the peer backend (slskd daemon or native
// soulseek client) the engine needs.
type PeerNetwork interface {
	ListDownloads(ctx context.Context) ([]core.RemoteTransfer, error)
	Cancel(ctx context.Context, username, id string) error
	// Remove purges a terminal transfer's leftover record from the backend
	// (slskd's DELETE ?remove=true, or the native client's registry+.part-file
	// cleanup). reconcile calls it immediately after the store marks a
	// transfer terminal, so the backend's transfer list does not accumulate
	// every finished download. It MUST run only after that store write —
	// otherwise the next reconcile pass would see the transfer gone from the
	// live list and mis-handle it as "lost".
	//
	// The Remove semantics differ by backend but converge on the same
	// observable property: slskd purges the daemon-side record, while the
	// native backend drops the transfer's registry entry and deletes its
	// .part file. Either way the transfer disappears from ListDownloads,
	// which is the only property the reconciler relies on.
	Remove(ctx context.Context, username, id string) error
}

// Ranker ranks search results into candidates (satisfied by matcher.Scorer).
type Ranker interface {
	Rank(results []core.SearchResult, rel map[string]core.PeerReliability, now time.Time) []core.RankedCandidate
}
