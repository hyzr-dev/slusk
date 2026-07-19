package core

import "encoding/json"

// SearchResult is one search result file offered by a peer, enriched with the
// peer's upload-availability signals. Provider-neutral: the pipeline and
// matcher consume this rather than a wire-specific type, so they have no
// dependency on any particular Soulseek client library. Filename preserves
// the provider's own path syntax (slskd uses "\" separators).
type SearchResult struct {
	Username          string
	Filename          string
	Size              int64
	BitRate           int
	HasFreeUploadSlot bool
	QueueLength       int
	UploadSpeed       int
}

// RankedCandidate is one user offering a group of files, with an aggregate
// score assigned by the matcher.
type RankedCandidate struct {
	Username string
	Files    []SearchResult
	Score    float64
}

// WantedRelease is one wanted/missing album from Lidarr, mapped to a
// music-source-neutral shape. Named WantedRelease rather than WantedAlbum
// because what Lidarr semantically wants is a release; its ID feeds
// AlbumJob.LidarrAlbumID.
type WantedRelease struct {
	ID         int64
	Title      string
	ArtistName string
	// ArtistID is Lidarr's artist id, cached onto AlbumJob so peer reliability
	// history (artist_user_reliability) can be keyed by artist rather than by
	// artist name, which can be renamed.
	ArtistID int64
	// ReleaseDate is Lidarr's raw release date/datetime string for the album.
	ReleaseDate string
}

// AlbumRelease is one release (edition/pressing) of an album in Lidarr, with
// its own track count, mapped to a music-source-neutral shape. Different
// releases of the same album legitimately have different track counts (bonus
// tracks, deluxe editions), and any of them is a valid import target since
// manual import runs with release switching enabled.
type AlbumRelease struct {
	ID         int64
	TrackCount int
	Monitored  bool
}

// ImportItem is one file Lidarr found in a folder, with any import
// rejections, mapped to a music-source-neutral shape.
type ImportItem struct {
	ID                      int64
	Path                    string
	ArtistID                int64
	AlbumID                 int64
	AlbumReleaseID          int64
	TrackIDs                []int64
	Quality                 json.RawMessage // opaque round-trip payload, echoed back to Lidarr byte-for-byte on import
	IndexerFlags            int64
	DisableReleaseSwitching bool
	Rejections              []string
	Importable              bool // true when Rejections is empty
}
