package core

import "errors"

// LidarrArtist is an artist in the user's Lidarr library, keyed by
// MusicBrainz artist id (foreignArtistId in Lidarr's own API). See
// internal/lidarr's ArtistByMBID, which looks an artist up in the user's
// library by this same id, and AddArtist, which creates one (issue #331).
type LidarrArtist struct {
	ID              int64
	ForeignArtistID string
	Name            string
	Monitored       bool
}

// LidarrRootFolder is one of Lidarr's configured root folders (issue #331).
// The "add artist" flow needs a target folder, and Lidarr's own UI prefills
// the profile selectors from DefaultQualityProfileID/DefaultMetadataProfileID
// once one is chosen - slskdarr needs no config keys of its own for these
// defaults, it just reads them from GET /rootfolder alongside everything
// else.
type LidarrRootFolder struct {
	ID                       int64
	Path                     string
	Accessible               bool
	FreeSpace                int64
	DefaultQualityProfileID  int64
	DefaultMetadataProfileID int64
}

// LidarrProfile is a Lidarr quality or metadata profile (issue #331). Both
// share this exact shape - id plus a display name - in Lidarr's own API, so
// one type serves both GET /qualityprofile and GET /metadataprofile.
type LidarrProfile struct {
	ID   int64
	Name string
}

// AddArtistRequest is what internal/lidarr.Client.AddArtist needs to create
// an artist (issue #331). It deliberately carries no monitoring intent -
// AddArtist always sends monitorNewItems:"none" and
// addOptions:{monitor:"none"} itself, regardless of what the caller wants
// monitored. See AddArtist's doc comment for why: on Lidarr 3.1.0,
// addOptions.monitor is inert, so intent must be expressed afterward via
// SetArtistMonitored and MonitorAlbums instead.
type AddArtistRequest struct {
	ForeignArtistID   string
	ArtistName        string
	QualityProfileID  int64
	MetadataProfileID int64
	RootFolderPath    string
}

// LidarrCommand is one entry in Lidarr's GET /command response (issue #331).
// AddArtistAndMonitor watches RefreshArtist/RefreshAlbum/RescanFolders here
// to avoid applying monitoring while Lidarr's asynchronous post-add refresh
// is still running - see testenv/seed_lidarr.py's wait_for_idle, which
// documents the exact race this mirrors: that refresh re-applies the
// artist's monitor policy and can silently undo monitoring set just before
// it ran.
//
// ArtistIDs is decoded from the command's body.artistIds - verified against
// the PR lab: a RefreshArtist triggered by our own add carries the exact
// artist id being added. A nil/empty ArtistIDs means the command is
// unscoped (a library-wide refresh, for instance), which can still touch
// our artist, so callers must treat that as blocking too - see
// app.LidarrLibrary.waitForIdle.
type LidarrCommand struct {
	Name      string
	Status    string
	ArtistIDs []int64
}

// ErrLidarrAddArtistUncertain is returned when Lidarr's artist-create call
// failed at the transport level and a follow-up check could not establish
// whether the write actually landed (issue #331 backend review). It lives
// here rather than in internal/lidarr so internal/app can detect it via
// errors.Is without importing that concrete adapter package - see
// internal/lidarr.Client.AddArtist, the only thing that returns it (as
// lidarr.ErrAddArtistUncertain, an alias of this same value).
var ErrLidarrAddArtistUncertain = errors.New("lidarr add artist: uncertain whether it succeeded")
