package core

// MBArtist is one MusicBrainz artist search result (issue #321). Score is
// MusicBrainz's own relevance score for the query, 0-100, passed through
// verbatim so the frontend can order results the same way MusicBrainz does.
type MBArtist struct {
	ID             string
	Name           string
	Type           string
	Country        string
	Disambiguation string
	Score          int
}

// MBReleaseGroup is one of an artist's albums (a MusicBrainz release-group).
// Its ID is the value Lidarr calls foreignAlbumId - see internal/lidarr's
// AlbumByForeignID, which looks a release-group up in the user's library by
// this same id.
type MBReleaseGroup struct {
	ID               string
	Title            string
	FirstReleaseDate string
	PrimaryType      string
	SecondaryTypes   []string
}

// MBRelease is one edition of a release-group, carrying its own track count
// rather than a min/max band: a release-group's editions legitimately range
// from a single-disc release to a multi-disc deluxe box set, and collapsing
// that into one band would hide exactly the distinction the modal needs the
// user to see (issue #321; Metallica's "Ride the Lightning" release-group
// spans 8..97 tracks across its 60 releases).
//
// TrackCountKnown is false when MusicBrainz reported no media data for this
// release at all - that must never be treated as a zero-track edition.
type MBRelease struct {
	ID              string
	Title           string
	Date            string
	Country         string
	Status          string
	TrackCount      int
	TrackCountKnown bool
}

// LidarrAlbumStatus is the read-only Lidarr library status for one
// MusicBrainz release-group (issue #321's identify modal). Known is false
// when Lidarr could not be reached or answered with an error - that must be
// surfaced to the user as "unknown", never silently treated as absent (the
// UI's MUSICBRAINZ UNAVAILABLE / LIDARR STATUS UNKNOWN distinction).
type LidarrAlbumStatus struct {
	Known     bool
	InLibrary bool
	// AlbumID is Lidarr's internal album id, meaningful only when InLibrary.
	AlbumID int64
}

// LidarrAlbum is the slice of a Lidarr library album internal/lidarr's
// AlbumByForeignID reports, keyed by MusicBrainz release-group id
// (foreignAlbumId in Lidarr's own API).
type LidarrAlbum struct {
	ID        int64
	ArtistID  int64
	Monitored bool
}

// LidarrArtist is the slice of a Lidarr library artist internal/lidarr's
// ArtistByMBID reports, keyed by MusicBrainz artist id (mbId in Lidarr's own
// API).
type LidarrArtist struct {
	ID        int64
	Monitored bool
}
