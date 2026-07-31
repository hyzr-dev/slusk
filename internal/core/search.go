package core

import (
	"encoding/json"
	"errors"
)

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

// AlbumTrack is one track of an album in Lidarr, mapped to a
// music-source-neutral shape, used by the discovery relevance gate to check a
// candidate's filenames against the album's real tracklist.
//
// Only Title is carried: an earlier version also decoded TrackNumber and
// MediumNumber, but nothing ever consumed them, so they were removed
// (YAGNI) - a type drift on either field in some deployed Lidarr version
// (e.g. trackNumber returned as a JSON number rather than a string) would
// otherwise fail the whole decode and degrade the relevance gate for every
// album, for fields nobody read. If a real need for track number comes back,
// keep it a string rather than parsing to int: vinyl releases use
// side/position labels like "A1", which is not an int.
type AlbumTrack struct {
	Title string
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

// RemoteTransfer is one file download a remote peer-to-peer provider (e.g.
// slskd) currently knows about, mapped to a provider-neutral shape.
type RemoteTransfer struct {
	ID        string
	Username  string
	Filename  string
	State     TransferState // the adapter maps the provider's own state strings onto this
	Size      int64
	BytesDone int64
	Failure   string // the provider's terminal failure text, if any
	// Retryable reports whether Failure is transient (worth re-queueing)
	// rather than permanent. Meaningful only when State is TransferErrored.
	Retryable bool
	// QueuePosition is the file's place in the peer's upload queue (0 when
	// unknown or not queued). Populated by providers that expose it natively
	// (the internal/soulseek downloader); the slskd adapter leaves it 0.
	QueuePosition uint32
	// Speed is the current transfer rate in bytes per second (0 when unknown).
	// Populated by providers that track it natively; the slskd adapter leaves
	// it 0.
	Speed int64
	// SpeedAverage is an EWMA-smoothed transfer rate in bytes per second (0
	// when unknown), backing ETA math (issue #157): Speed is instantaneous and
	// jumpy — dividing remaining bytes by it would make an ETA flicker wildly
	// on every sample, so ETA is computed from this smoothed figure instead.
	// Populated only by the native soulseek downloader; the slskd adapter
	// leaves it 0.
	SpeedAverage int64
}

// ErrRemoteNotFound is returned (wrapped) by a peer-to-peer provider adapter
// when the provider reports a resource (e.g. a transfer or download folder)
// as not found — typically a routine outcome (e.g. the provider already
// forgot a terminal transfer), not a real failure.
var ErrRemoteNotFound = errors.New("remote resource not found")
