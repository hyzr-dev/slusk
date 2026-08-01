// Package app: lidarr_library.go is the transport-neutral service backing
// issue #331's "add to Lidarr" flow - checking whether an artist/album a
// user is about to manually download is already in their Lidarr library,
// and if not, adding the artist on their behalf.
//
// Nothing here monitors anything. Monitoring an album with no files puts it
// in Lidarr's wanted/missing list, which is exactly what pipeline.WantedSync
// polls - measured in the PR lab, a manual job and a duplicate
// Lidarr-sourced job for the same album were created three seconds apart,
// then raced to download into the same folder. Lidarr's manual import does
// not care about the monitored flag (verified: GET /manualimport matched all
// tracks with zero rejections, and POST /command{ManualImport} imported them,
// for an unmonitored album), so the add stays unmonitored and issue #59's
// import step resolves the album id from the MBID at import time.
//
// LidarrLibrary declares its own narrow interface onto internal/lidarr,
// exactly like Identify declares LidarrLibraryLookup: this package owns the
// interface it consumes, not the package it wraps.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// ErrLidarrLibraryQueryInvalid is returned when a required argument is
// blank or non-positive; internal/observ maps this to 422.
var ErrLidarrLibraryQueryInvalid = errors.New("lidarr library request is missing a required field")

// ErrLidarrLibraryInvalidRootFolder is returned when
// AddArtistParams.RootFolderPath is not one of Lidarr's currently configured,
// accessible root folders (issue #331 backend review); internal/observ maps
// this to 422 with a rootFolderPath field error. RootFolderPath is the one
// user-controlled string in this flow that reaches an upstream write
// unchecked, and the allowed set is one GET /rootfolder away.
var ErrLidarrLibraryInvalidRootFolder = errors.New("root folder path is not one of Lidarr's configured, accessible root folders")

// ErrLidarrLibraryAddUncertain is an alias of core.ErrLidarrAddArtistUncertain
// (issue #331 backend review): the artist-create request failed at the
// transport level and Lidarr's own re-check could not establish whether the
// write actually landed. internal/observ maps this to its own distinct
// status (502 with code "addUncertain") rather than the generic 500 every
// other failure gets, since a blind client retry here risks creating a
// duplicate artist. Exposed under this name so callers outside internal/core
// don't need to know the sentinel originates from Lidarr's own client.
var ErrLidarrLibraryAddUncertain = core.ErrLidarrAddArtistUncertain

// LidarrArtistStatus is the read-only Lidarr library status for one
// MusicBrainz artist (issue #331), mirroring app.LidarrAlbumStatus's
// three-way semantics exactly: Known is false when Lidarr could not be
// reached or answered with an error - that must be surfaced to the user as
// "unknown", never silently treated as absent.
type LidarrArtistStatus struct {
	Known     bool
	InLibrary bool
	// ArtistID and Name are meaningful only when InLibrary.
	ArtistID int64
	Name     string
}

// LidarrAddOptions carries everything the "add artist" form needs to
// populate its selectors (issue #331): every configured root folder (with
// its default profile ids) plus every quality and metadata profile. No
// config keys back this - Lidarr's own GET /rootfolder response already
// carries the per-folder defaults the UI prefills from.
type LidarrAddOptions struct {
	RootFolders      []core.LidarrRootFolder
	QualityProfiles  []core.LidarrProfile
	MetadataProfiles []core.LidarrProfile
}

// AddArtistParams configures EnsureArtist.
type AddArtistParams struct {
	// ArtistMBID is the MusicBrainz artist id to add (Lidarr's foreignArtistId).
	ArtistMBID string
	// ArtistName is passed through to Lidarr's create call for display; it
	// is not used to look anything up.
	ArtistName        string
	RootFolderPath    string
	QualityProfileID  int64
	MetadataProfileID int64
}

// AddArtistResult reports what EnsureArtist actually did (issue #331).
// AlreadyInLibrary being true means no artist was created - the existing one
// was reused and nothing about it was touched.
//
// It carries no monitoring facts because nothing is monitored any more (see
// the package doc comment): a field reporting a state this flow no longer
// produces would be an invented one.
type AddArtistResult struct {
	ArtistID         int64
	AlreadyInLibrary bool
}

// LidarrLibraryClient is the slice of internal/lidarr.Client LidarrLibrary
// needs for issue #331's "add to Lidarr" flow.
type LidarrLibraryClient interface {
	ArtistByMBID(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error)
	RootFolders(ctx context.Context) ([]core.LidarrRootFolder, error)
	QualityProfiles(ctx context.Context) ([]core.LidarrProfile, error)
	MetadataProfiles(ctx context.Context) ([]core.LidarrProfile, error)
	AddArtist(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error)
}

// LidarrLibraryParams configures NewLidarrLibrary.
type LidarrLibraryParams struct {
	Lidarr LidarrLibraryClient
	Logger *slog.Logger
}

// LidarrLibrary is the transport-neutral service backing issue #331's "add
// to Lidarr" flow.
type LidarrLibrary struct {
	lidarr LidarrLibraryClient
	logger *slog.Logger
}

// NewLidarrLibrary constructs a LidarrLibrary service.
func NewLidarrLibrary(p LidarrLibraryParams) *LidarrLibrary {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &LidarrLibrary{lidarr: p.Lidarr, logger: logger}
}

// ArtistStatus reports whether a MusicBrainz artist is already in the
// user's Lidarr library, for GET /api/lidarr/artists/{mbid}. A Lidarr
// transport failure is not mapped to an error - like
// Identify.AlbumLidarrStatus, "Lidarr unreachable" is a normal, displayable
// outcome (Known = false), not a request failure.
func (l *LidarrLibrary) ArtistStatus(ctx context.Context, artistMBID string) (LidarrArtistStatus, error) {
	artistMBID = strings.TrimSpace(artistMBID)
	if artistMBID == "" {
		return LidarrArtistStatus{}, ErrLidarrLibraryQueryInvalid
	}
	artist, found, err := l.lidarr.ArtistByMBID(ctx, artistMBID)
	if err != nil {
		l.logger.Warn("lidarr artist lookup failed", "artistMBID", artistMBID, "err", err)
		return LidarrArtistStatus{Known: false}, nil
	}
	if !found {
		return LidarrArtistStatus{Known: true, InLibrary: false}, nil
	}
	return LidarrArtistStatus{Known: true, InLibrary: true, ArtistID: artist.ID, Name: artist.Name}, nil
}

// AddOptions gathers everything the "add artist" form needs (issue #331),
// for GET /api/lidarr/add-options.
func (l *LidarrLibrary) AddOptions(ctx context.Context) (LidarrAddOptions, error) {
	roots, err := l.lidarr.RootFolders(ctx)
	if err != nil {
		return LidarrAddOptions{}, fmt.Errorf("lidarr root folders: %w", err)
	}
	quality, err := l.lidarr.QualityProfiles(ctx)
	if err != nil {
		return LidarrAddOptions{}, fmt.Errorf("lidarr quality profiles: %w", err)
	}
	metadata, err := l.lidarr.MetadataProfiles(ctx)
	if err != nil {
		return LidarrAddOptions{}, fmt.Errorf("lidarr metadata profiles: %w", err)
	}
	return LidarrAddOptions{RootFolders: roots, QualityProfiles: quality, MetadataProfiles: metadata}, nil
}

// findOrCreateArtist returns the library artist id for params.ArtistMBID,
// creating it if it does not already exist. alreadyInLibrary is true when an
// existing artist was reused - EnsureArtist must not re-create it, and must
// not touch it in any other way either.
//
// A transport error from AddArtist that wraps core.ErrLidarrAddArtistUncertain
// is returned unwrapped further, so errors.Is(err, ErrLidarrLibraryAddUncertain)
// keeps working all the way out to internal/observ.
func (l *LidarrLibrary) findOrCreateArtist(ctx context.Context, params AddArtistParams) (artistID int64, alreadyInLibrary bool, err error) {
	existing, found, err := l.lidarr.ArtistByMBID(ctx, params.ArtistMBID)
	if err != nil {
		return 0, false, fmt.Errorf("lidarr artist lookup: %w", err)
	}
	if found {
		return existing.ID, true, nil
	}
	created, err := l.lidarr.AddArtist(ctx, core.AddArtistRequest{
		ForeignArtistID:   params.ArtistMBID,
		ArtistName:        params.ArtistName,
		QualityProfileID:  params.QualityProfileID,
		MetadataProfileID: params.MetadataProfileID,
		RootFolderPath:    params.RootFolderPath,
	})
	if err != nil {
		if errors.Is(err, ErrLidarrLibraryAddUncertain) {
			return 0, false, err
		}
		return 0, false, fmt.Errorf("lidarr add artist: %w", err)
	}
	return created.ID, false, nil
}

// rootFolderAllowed reports whether path matches one of Lidarr's currently
// configured, accessible root folders (issue #331 backend review #7) -
// RootFolderPath is the one user-controlled string in this flow that reaches
// an upstream write unchecked.
func rootFolderAllowed(path string, roots []core.LidarrRootFolder) bool {
	for _, r := range roots {
		if r.Path == path && r.Accessible {
			return true
		}
	}
	return false
}

// EnsureArtist makes sure the artist behind params.ArtistMBID exists in the
// user's Lidarr library, creating it - unmonitored - if it does not (issue
// #331, reshaped by the #59 follow-up). Nothing is monitored, here or in
// internal/lidarr's AddArtist: see the package doc comment for the measured
// reason. An artist that already exists is reused untouched.
func (l *LidarrLibrary) EnsureArtist(ctx context.Context, params AddArtistParams) (AddArtistResult, error) {
	if strings.TrimSpace(params.ArtistMBID) == "" ||
		strings.TrimSpace(params.RootFolderPath) == "" ||
		params.QualityProfileID <= 0 ||
		params.MetadataProfileID <= 0 {
		return AddArtistResult{}, ErrLidarrLibraryQueryInvalid
	}

	roots, err := l.lidarr.RootFolders(ctx)
	if err != nil {
		return AddArtistResult{}, fmt.Errorf("lidarr root folders: %w", err)
	}
	if !rootFolderAllowed(params.RootFolderPath, roots) {
		return AddArtistResult{}, ErrLidarrLibraryInvalidRootFolder
	}

	artistID, alreadyInLibrary, err := l.findOrCreateArtist(ctx, params)
	if err != nil {
		return AddArtistResult{}, err
	}
	return AddArtistResult{ArtistID: artistID, AlreadyInLibrary: alreadyInLibrary}, nil
}
