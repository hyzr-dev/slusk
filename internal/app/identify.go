// Package app: identify.go is the transport-neutral service backing issue
// #321's identify modal - the third button on a manual search result card
// that lets a user explicitly identify a Soulseek result against MusicBrainz
// and see its read-only Lidarr library status, without inventing an artist
// from the peer's folder structure (see internal/observ/search.go, whose
// group.Parent is a best-effort guess, never a canonical identity).
//
// Identify declares its own narrow interfaces onto internal/musicbrainz and
// internal/lidarr, exactly like Searches declares PeerStreamSearcher: this
// package owns the interfaces it consumes, not the packages it wraps.
package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// ErrIdentifyQueryInvalid is returned when a required query or id argument is
// blank; internal/observ maps this to 422.
var ErrIdentifyQueryInvalid = errors.New("identify query is required")

// ErrIdentifyUnavailable is returned when MusicBrainz could not be reached or
// rejected the request (rate limited, non-2xx, etc). internal/observ maps
// this to 503 so the frontend can show its MUSICBRAINZ UNAVAILABLE state
// distinctly from a real (500) error.
var ErrIdentifyUnavailable = errors.New("musicbrainz is not available")

// MusicBrainzSearcher is the slice of internal/musicbrainz.Client Identify
// needs.
type MusicBrainzSearcher interface {
	SearchArtists(ctx context.Context, query string) ([]core.MBArtist, error)
	ReleaseGroups(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, error)
	Releases(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, error)
}

// LidarrLibraryLookup is the slice of internal/lidarr.Client Identify needs
// for the read-only Lidarr status row.
type LidarrLibraryLookup interface {
	AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
}

// IdentifyParams configures NewIdentify.
type IdentifyParams struct {
	MusicBrainz MusicBrainzSearcher
	Lidarr      LidarrLibraryLookup
	Logger      *slog.Logger
}

// Identify is the transport-neutral service backing issue #321's identify
// modal: MusicBrainz artist/release-group/release lookups, plus the
// read-only Lidarr library status for a chosen release-group.
type Identify struct {
	mb     MusicBrainzSearcher
	lidarr LidarrLibraryLookup
	logger *slog.Logger
}

// NewIdentify constructs an Identify service.
func NewIdentify(p IdentifyParams) *Identify {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Identify{mb: p.MusicBrainz, lidarr: p.Lidarr, logger: logger}
}

// SearchArtists searches MusicBrainz artists by free-text query, for GET
// /api/identify/artists.
func (id *Identify) SearchArtists(ctx context.Context, query string) ([]core.MBArtist, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, ErrIdentifyQueryInvalid
	}
	artists, err := id.mb.SearchArtists(ctx, query)
	if err != nil {
		id.logger.Warn("musicbrainz artist search failed", "err", err)
		return nil, ErrIdentifyUnavailable
	}
	return artists, nil
}

// ArtistAlbums lists an artist's release-groups, for GET
// /api/identify/artists/{mbid}/albums.
func (id *Identify) ArtistAlbums(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, error) {
	artistMBID = strings.TrimSpace(artistMBID)
	if artistMBID == "" {
		return nil, ErrIdentifyQueryInvalid
	}
	groups, err := id.mb.ReleaseGroups(ctx, artistMBID)
	if err != nil {
		id.logger.Warn("musicbrainz release-group lookup failed", "artistMBID", artistMBID, "err", err)
		return nil, ErrIdentifyUnavailable
	}
	return groups, nil
}

// AlbumEditions lists a release-group's editions, each with its own
// per-edition track count (see core.MBRelease), for GET
// /api/identify/albums/{mbid}/editions.
func (id *Identify) AlbumEditions(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, error) {
	releaseGroupMBID = strings.TrimSpace(releaseGroupMBID)
	if releaseGroupMBID == "" {
		return nil, ErrIdentifyQueryInvalid
	}
	releases, err := id.mb.Releases(ctx, releaseGroupMBID)
	if err != nil {
		id.logger.Warn("musicbrainz release lookup failed", "releaseGroupMBID", releaseGroupMBID, "err", err)
		return nil, ErrIdentifyUnavailable
	}
	return releases, nil
}

// AlbumLidarrStatus reports whether a release-group is already in the user's
// Lidarr library, for GET /api/identify/albums/{mbid}/lidarr. Unlike the
// MusicBrainz-backed methods above, a Lidarr transport failure is not mapped
// to an error: the design ("Lidarr onåbar -> okänt", issue #321) treats
// "Lidarr unreachable" as a normal, displayable outcome - core.LidarrAlbumStatus.Known
// = false - not a request failure, so the UI can say LIDARR STATUS UNKNOWN
// rather than 503ing the whole request.
func (id *Identify) AlbumLidarrStatus(ctx context.Context, releaseGroupMBID string) (core.LidarrAlbumStatus, error) {
	releaseGroupMBID = strings.TrimSpace(releaseGroupMBID)
	if releaseGroupMBID == "" {
		return core.LidarrAlbumStatus{}, ErrIdentifyQueryInvalid
	}
	album, found, err := id.lidarr.AlbumByForeignID(ctx, releaseGroupMBID)
	if err != nil {
		id.logger.Warn("lidarr album lookup failed", "releaseGroupMBID", releaseGroupMBID, "err", err)
		return core.LidarrAlbumStatus{Known: false}, nil
	}
	if !found {
		return core.LidarrAlbumStatus{Known: true, InLibrary: false}, nil
	}
	return core.LidarrAlbumStatus{Known: true, InLibrary: true, AlbumID: album.ID}, nil
}
