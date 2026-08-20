package core

import (
	"encoding/json"
	"errors"
	"time"
)

// SearchResult is one search result file offered by a peer, enriched with the
// peer's upload-availability signals. Provider-neutral: the pipeline and
// matcher consume this rather than a wire-specific type, so they have no
// dependency on any particular Soulseek client library. Filename preserves
// the provider's own path syntax (slskd uses "\" separators).
//
// Duration/SampleRate/BitDepth/VariableBitRate are the peer's optional file
// attributes (issue #58). Zero means "the peer sent no such attribute" for
// Duration/SampleRate/BitDepth — a 0-second track, a 0 Hz sample rate, and a
// 0-bit depth are all physically impossible for a real audio file, so zero
// unambiguously reads as unknown. This is deliberately not distinguished from
// "the peer reported zero": a *int would push nil-checks through every
// adapter, the session grouping, and the TypeScript for no informational
// gain.
type SearchResult struct {
	Username          string
	Filename          string
	Size              int64
	BitRate           int
	HasFreeUploadSlot bool
	QueueLength       int
	UploadSpeed       int
	Duration          int  // seconds; 0 = the peer sent no duration attribute
	SampleRate        int  // Hz; 0 = unknown
	BitDepth          int  // bits; 0 = unknown
	VariableBitRate   bool // the peer's VBR attribute (code 2)
}

// SearchFile is one file of a SearchGroup, the display-ready shape a manual
// search session (issue #58) exposes per file. Filename is the full
// peer-syntax path — exactly what POST /api/jobs requires to enqueue it; Name
// is its display basename.
type SearchFile struct {
	Filename        string
	Name            string
	Size            int64
	BitRate         int
	Duration        int
	SampleRate      int
	BitDepth        int
	VariableBitRate bool
}

// SearchGroup is one release — one (peer, release directory) pair — offered
// by a manual search (issue #58), aggregating its files and the peer's
// upload-availability signals. ID is a stable, opaque identifier
// (sha256(username + "\x00" + releaseDir)[:16] hex) safe to use as a JSON
// value and a React list key. Version is a per-session monotonic counter
// bumped every time this group's file set changes, letting SearchDelta report
// exactly which groups changed since a caller's cursor.
type SearchGroup struct {
	ID   string
	Peer string
	// Folder is the peer's release directory, SLASH-separated: it comes from
	// matcher.ReleaseDir, which normalizes the peer's native "\" separators to
	// "/". A peer path that arrives as `@@abc\Music\Radiohead\In Rainbows` is
	// therefore stored as "@@abc/Music/Radiohead/In Rainbows".
	Folder          string
	Title           string // path.Base(Folder)
	Parent          string // path.Base(path.Dir(Folder)) — the peer's folder, not a resolved artist
	TrackCount      int
	SizeBytes       int64
	DurationSeconds int // sum of Files' Duration; 0 when not every file reported one
	Format          string
	BitRate         int
	SampleRate      int
	BitDepth        int
	VariableBitRate bool
	FreeUploadSlot  bool
	QueueLength     int
	UploadSpeed     int
	Score           float64
	Version         int
	Files           []SearchFile
}

// SearchSession is the whole state of one manual search (issue #58): the
// truth source served by GET /api/search/{id} and the base a SearchDelta is
// computed against. Groups is the complete current set, not a delta.
type SearchSession struct {
	ID        string
	Query     string
	StartedAt time.Time
	Done      bool
	// Streaming reports whether results genuinely arrive incrementally
	// (native backend: true) or only as one batch at completion (slskd: see
	// internal/slskd's SearchStreaming).
	Streaming bool
	// Truncated is set once the session's result cap (searchMaxResults) is
	// reached — further results are dropped rather than silently miscounted.
	Truncated bool
	// Err is the backend failure that ended the search early, if any. Partial
	// results (Groups) are retained rather than discarded.
	Err    string
	Total  int // accepted results so far, across every group
	Groups []SearchGroup
}

// SearchDelta is the incremental shape served over SSE (`event: search`,
// issue #58): every SearchGroup that changed since a subscriber's cursor
// (Seq), not the whole session. A changed group is always resent whole, never
// as a file-level diff, so grouping/scoring logic exists in exactly one place
// (Go) rather than being reimplemented in TypeScript.
type SearchDelta struct {
	ID        string
	Seq       int // cursor to pass as `since` on the next call
	Groups    []SearchGroup
	Total     int
	Done      bool
	Streaming bool
	Truncated bool
	Err       string
}

// RankedCandidate is one user offering a group of files, with an aggregate
// score assigned by the matcher.
type RankedCandidate struct {
	Username string
	Files    []SearchResult
	Score    float64
	// LastResort marks a candidate whose peer history was bad enough at
	// ranking time to sort it behind every candidate whose was not, whatever
	// its Score (issue #508, see matcher.IsLastResortPeer). It is an ordering
	// signal, never a filter: a last-resort candidate that is the only one an
	// album has is still selected.
	LastResort bool
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
	Rejections              []ImportRejection
	Importable              bool // true when Rejections is empty
}

// ImportRejection is one reason Lidarr gave for refusing a file, with the
// permanence Lidarr assigned it.
//
// Permanent is Lidarr's own verdict that the answer will not change on a
// retry, and is what lets a job reach StateImportRefused rather than cycling
// through every remaining candidate for an album none of them can satisfy.
//
// Read it knowing how Lidarr produces it: `Rejection(string reason,
// RejectionType type = RejectionType.Permanent)` defaults to permanent, so a
// rejection built from a reason alone arrives here as Permanent whether or not
// it describes something durable. "Not enough free space" and "File is still
// being unpacked" are both constructed that way. Issue #470 decided to act on
// the flag as given regardless; a caller that needs to distinguish those has to
// read Reason.
type ImportRejection struct {
	Reason    string
	Permanent bool
}

// Reasons flattens rejections to their text, for logging and event details
// where the permanence is not what the reader needs.
func Reasons(rejections []ImportRejection) []string {
	out := make([]string, 0, len(rejections))
	for _, r := range rejections {
		out = append(out, r.Reason)
	}
	return out
}

// AnyPermanent reports whether any rejection carries Lidarr's Permanent
// verdict. Any, not all: a single durable reason is enough to make retrying
// the same folder pointless, and Lidarr stamps folder-level reasons onto every
// file it could not place.
func AnyPermanent(rejections []ImportRejection) bool {
	for _, r := range rejections {
		if r.Permanent {
			return true
		}
	}
	return false
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

// ErrSearchExcluded is returned (wrapped) by a peer-to-peer provider adapter
// when a search query covers a phrase the Soulseek server has told clients to
// exclude (server code 160): well-behaved peers refuse to answer any search
// whose terms cover an excluded phrase, so the query would return zero
// results from every peer no matter how many times it is retried. Callers
// should treat this as a permanent, non-retryable outcome for that query
// rather than backing off and reissuing it.
var ErrSearchExcluded = errors.New("search query covers a server-excluded phrase")
