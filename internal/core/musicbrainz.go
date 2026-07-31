package core

// MBReleaseGroup is one release-group hit from MusicBrainz's combined
// artist+album search (issue #321). Its ID is the value Lidarr calls
// foreignAlbumId - see internal/lidarr's AlbumByForeignID, which looks a
// release-group up in the user's library by this same id.
//
// ArtistName/ArtistID come from the hit's first artist-credit entry, so a
// search result row can show and link its artist without a second lookup.
// EditionCount is MusicBrainz's own "count" field on the hit - the number of
// releases (editions) in the release-group, exactly what
// internal/musicbrainz.Client.Releases would return the length of, without
// paying for that second request. Score is MusicBrainz's own relevance
// score for the query, 0-100, passed through verbatim so the frontend can
// order results the same way MusicBrainz does.
type MBReleaseGroup struct {
	ID               string
	Title            string
	ArtistName       string
	ArtistID         string
	FirstReleaseDate string
	PrimaryType      string
	SecondaryTypes   []string
	EditionCount     int
	Score            int
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

// LidarrAlbum is the slice of a Lidarr library album internal/lidarr's
// AlbumByForeignID reports, keyed by MusicBrainz release-group id
// (foreignAlbumId in Lidarr's own API).
type LidarrAlbum struct {
	ID        int64
	ArtistID  int64
	Monitored bool
}
