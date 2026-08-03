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

	"github.com/hyzr-dev/slusk/internal/core"
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
// needs. Each method's int return is MusicBrainz's true match count, which
// may exceed len(slice) when the result was capped - see
// internal/musicbrainz.Client's per-method doc comments.
type MusicBrainzSearcher interface {
	SearchReleaseGroups(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error)
	Releases(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error)
}

// LidarrLibraryLookup is the slice of internal/lidarr.Client Identify needs
// for the read-only Lidarr status row.
type LidarrLibraryLookup interface {
	AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error)
}

// LidarrAlbumStatus is the read-only Lidarr library status for one
// MusicBrainz release-group (issue #321's identify modal). Known is false
// when Lidarr could not be reached or answered with an error - that must be
// surfaced to the user as "unknown", never silently treated as absent (the
// UI's MUSICBRAINZ UNAVAILABLE / LIDARR STATUS UNKNOWN distinction). It
// lives here rather than in internal/core because it is constructed only by
// Identify.AlbumLidarrStatus and consumed only by internal/observ - no
// adapter maps a wire type to it.
type LidarrAlbumStatus struct {
	Known     bool
	InLibrary bool
	// AlbumID is Lidarr's internal album id, meaningful only when InLibrary.
	AlbumID int64
}

// IdentifyParams configures NewIdentify.
type IdentifyParams struct {
	MusicBrainz MusicBrainzSearcher
	Lidarr      LidarrLibraryLookup
	Logger      *slog.Logger
}

// Identify is the transport-neutral service backing issue #321's identify
// modal: a combined MusicBrainz artist+album search, per-release-group
// edition lookups, plus the read-only Lidarr library status for a chosen
// release-group.
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

// SearchReleaseGroups runs a combined artist+album search against
// MusicBrainz, for GET /api/identify/search - see
// internal/musicbrainz.Client.SearchReleaseGroups for the query it builds
// and the fuzzy retry it performs on a miss. artist may be blank (the
// caller degrades to an album-only search); album may not - there is
// nothing to search for otherwise, so a blank album is
// ErrIdentifyQueryInvalid before any request is made. total is
// MusicBrainz's true match count and may exceed len(results) when the
// result was capped - see MusicBrainzSearcher.
func (id *Identify) SearchReleaseGroups(ctx context.Context, artist, album string) (results []core.MBReleaseGroup, total int, err error) {
	artist = strings.TrimSpace(artist)
	album = strings.TrimSpace(album)
	if album == "" {
		return nil, 0, ErrIdentifyQueryInvalid
	}
	results, total, err = id.mb.SearchReleaseGroups(ctx, artist, album)
	if err != nil {
		id.logger.Warn("musicbrainz release-group search failed", "err", err)
		return nil, 0, ErrIdentifyUnavailable
	}
	return results, total, nil
}

// AlbumEditions lists a release-group's editions, each with its own
// per-edition track count (see core.MBRelease), for GET
// /api/identify/albums/{mbid}/editions. total is MusicBrainz's true match
// count and may exceed len(releases) when the result was capped - see
// MusicBrainzSearcher.
func (id *Identify) AlbumEditions(ctx context.Context, releaseGroupMBID string) (releases []core.MBRelease, total int, err error) {
	releaseGroupMBID = strings.TrimSpace(releaseGroupMBID)
	if releaseGroupMBID == "" {
		return nil, 0, ErrIdentifyQueryInvalid
	}
	releases, total, err = id.mb.Releases(ctx, releaseGroupMBID)
	if err != nil {
		id.logger.Warn("musicbrainz release lookup failed", "releaseGroupMBID", releaseGroupMBID, "err", err)
		return nil, 0, ErrIdentifyUnavailable
	}
	return releases, total, nil
}

// AlbumLidarrStatus reports whether a release-group is already in the user's
// Lidarr library, for GET /api/identify/albums/{mbid}/lidarr. Unlike the
// MusicBrainz-backed methods above, a Lidarr transport failure is not mapped
// to an error: the design (issue #321 - an unreachable Lidarr is reported as
// unknown, never as "not in library") treats "Lidarr unreachable" as a
// normal, displayable outcome - LidarrAlbumStatus.Known = false - not a
// request failure, so the UI can say LIDARR STATUS UNKNOWN rather than
// 503ing the whole request.
func (id *Identify) AlbumLidarrStatus(ctx context.Context, releaseGroupMBID string) (LidarrAlbumStatus, error) {
	releaseGroupMBID = strings.TrimSpace(releaseGroupMBID)
	if releaseGroupMBID == "" {
		return LidarrAlbumStatus{}, ErrIdentifyQueryInvalid
	}
	album, found, err := id.lidarr.AlbumByForeignID(ctx, releaseGroupMBID)
	if err != nil {
		id.logger.Warn("lidarr album lookup failed", "releaseGroupMBID", releaseGroupMBID, "err", err)
		return LidarrAlbumStatus{Known: false}, nil
	}
	if !found {
		return LidarrAlbumStatus{Known: true, InLibrary: false}, nil
	}
	return LidarrAlbumStatus{Known: true, InLibrary: true, AlbumID: album.ID}, nil
}
