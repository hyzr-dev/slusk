// Package observ: lidarr.go serves issue #331's "add to Lidarr" flow: GET
// /api/lidarr/artists/{mbid} reports the read-only artist library status,
// GET /api/lidarr/add-options supplies the root folders and profiles the
// "add artist" form needs, and POST /api/lidarr/artists creates the artist
// and monitors the chosen album(s). All three are nil-safe, mirroring
// registerIdentify: when the wiring is absent, every field is nil and every
// endpoint answers 503 instead of panicking.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/app"
)

// LidarrArtistStatusFunc reports the read-only Lidarr library status for a
// MusicBrainz artist (typically backed by app.LidarrLibrary.ArtistStatus)
// for GET /api/lidarr/artists/{mbid}.
type LidarrArtistStatusFunc func(ctx context.Context, artistMBID string) (app.LidarrArtistStatus, error)

// LidarrAddOptionsFunc supplies the "add artist" form's root folders and
// profiles (typically backed by app.LidarrLibrary.AddOptions) for GET
// /api/lidarr/add-options.
type LidarrAddOptionsFunc func(ctx context.Context) (app.LidarrAddOptions, error)

// LidarrAddArtistFunc ensures an artist exists in the Lidarr library
// (typically backed by app.LidarrLibrary.EnsureArtist) for POST
// /api/lidarr/artists.
type LidarrAddArtistFunc func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error)

// lidarrArtistStatusDTO is the JSON shape of app.LidarrArtistStatus.
// artistId/name are omitted when the artist is not in the library (or
// unknown).
type lidarrArtistStatusDTO struct {
	Known     bool   `json:"known"`
	InLibrary bool   `json:"inLibrary"`
	ArtistID  int64  `json:"artistId,omitempty"`
	Name      string `json:"name,omitempty"`
}

func toLidarrArtistStatusDTO(s app.LidarrArtistStatus) lidarrArtistStatusDTO {
	return lidarrArtistStatusDTO{Known: s.Known, InLibrary: s.InLibrary, ArtistID: s.ArtistID, Name: s.Name}
}

// lidarrRootFolderDTO is the JSON shape of one core.LidarrRootFolder.
type lidarrRootFolderDTO struct {
	ID                       int64  `json:"id"`
	Path                     string `json:"path"`
	Accessible               bool   `json:"accessible"`
	FreeSpace                int64  `json:"freeSpace"`
	DefaultQualityProfileID  int64  `json:"defaultQualityProfileId"`
	DefaultMetadataProfileID int64  `json:"defaultMetadataProfileId"`
}

// lidarrProfileDTO is the JSON shape of one core.LidarrProfile.
type lidarrProfileDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// lidarrAddOptionsDTO is the JSON shape of GET /api/lidarr/add-options.
type lidarrAddOptionsDTO struct {
	RootFolders      []lidarrRootFolderDTO `json:"rootFolders"`
	QualityProfiles  []lidarrProfileDTO    `json:"qualityProfiles"`
	MetadataProfiles []lidarrProfileDTO    `json:"metadataProfiles"`
}

func toLidarrAddOptionsDTO(o app.LidarrAddOptions) lidarrAddOptionsDTO {
	roots := make([]lidarrRootFolderDTO, len(o.RootFolders))
	for i, r := range o.RootFolders {
		roots[i] = lidarrRootFolderDTO{
			ID: r.ID, Path: r.Path, Accessible: r.Accessible, FreeSpace: r.FreeSpace,
			DefaultQualityProfileID: r.DefaultQualityProfileID, DefaultMetadataProfileID: r.DefaultMetadataProfileID,
		}
	}
	quality := make([]lidarrProfileDTO, len(o.QualityProfiles))
	for i, p := range o.QualityProfiles {
		quality[i] = lidarrProfileDTO{ID: p.ID, Name: p.Name}
	}
	metadata := make([]lidarrProfileDTO, len(o.MetadataProfiles))
	for i, p := range o.MetadataProfiles {
		metadata[i] = lidarrProfileDTO{ID: p.ID, Name: p.Name}
	}
	return lidarrAddOptionsDTO{RootFolders: roots, QualityProfiles: quality, MetadataProfiles: metadata}
}

// addArtistRequest is the JSON body of POST /api/lidarr/artists. It carries
// no album id and no monitoring choice: the add only ensures the artist
// exists, unmonitored - see internal/app/lidarr_library.go's package doc
// comment for why monitoring was removed entirely.
type addArtistRequest struct {
	ArtistMBID        string `json:"artistMbid"`
	ArtistName        string `json:"artistName"`
	RootFolderPath    string `json:"rootFolderPath"`
	QualityProfileID  int64  `json:"qualityProfileId"`
	MetadataProfileID int64  `json:"metadataProfileId"`
}

// addArtistResultDTO is the JSON shape of app.AddArtistResult, the 201 body
// of POST /api/lidarr/artists.
type addArtistResultDTO struct {
	ArtistID         int64 `json:"artistId"`
	AlreadyInLibrary bool  `json:"alreadyInLibrary"`
}

// validateAddArtistRequest validates req, mirroring
// validateCreateJobRequest's field-keyed error style.
func validateAddArtistRequest(req addArtistRequest) (params app.AddArtistParams, fieldErrors map[string]string) {
	fieldErrors = make(map[string]string)
	if strings.TrimSpace(req.ArtistMBID) == "" {
		fieldErrors["artistMbid"] = "is required"
	}
	if strings.TrimSpace(req.ArtistName) == "" {
		fieldErrors["artistName"] = "is required"
	}
	if strings.TrimSpace(req.RootFolderPath) == "" {
		fieldErrors["rootFolderPath"] = "is required"
	}
	if req.QualityProfileID <= 0 {
		fieldErrors["qualityProfileId"] = "must be > 0"
	}
	if req.MetadataProfileID <= 0 {
		fieldErrors["metadataProfileId"] = "must be > 0"
	}
	return app.AddArtistParams{
		ArtistMBID: req.ArtistMBID, ArtistName: req.ArtistName,
		RootFolderPath: req.RootFolderPath, QualityProfileID: req.QualityProfileID,
		MetadataProfileID: req.MetadataProfileID,
	}, fieldErrors
}

// writeLidarrLibraryError maps a LidarrLibrary service error to a status
// code and writes it:
//   - errors.Is(err, app.ErrLidarrLibraryQueryInvalid) -> 422. This error
//     covers four different required-or-invalid fields (mbid,
//     rootFolderPath, qualityProfileId, metadataProfileId) depending on
//     which endpoint and code path produced it, so unlike the other cases
//     below it cannot honestly name a single offending field - see issue
//     #331 backend review #8, which replaced a hardcoded (and often wrong)
//     "mbid" guess with this generic message instead.
//   - errors.Is(err, app.ErrLidarrLibraryInvalidRootFolder) -> 422 with a
//     rootFolderPath field error.
//   - errors.Is(err, app.ErrLidarrLibraryAddUncertain) -> 502 with
//     code "addUncertain", distinct from every other failure: the create may
//     or may not have landed, so a client must not blindly retry (which
//     risks a duplicate artist) the way it safely could for a plain 500.
//   - anything else -> 500.
func writeLidarrLibraryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrLidarrLibraryQueryInvalid):
		writeConfigError(w, http.StatusUnprocessableEntity, "one or more required fields are missing or invalid", nil)
	case errors.Is(err, app.ErrLidarrLibraryInvalidRootFolder):
		writeConfigError(w, http.StatusUnprocessableEntity, "validation failed", map[string]string{"rootFolderPath": "is not a configured, accessible root folder"})
	case errors.Is(err, app.ErrLidarrLibraryAddUncertain):
		writeConfigErrorWithCode(w, http.StatusBadGateway, err.Error(), "addUncertain")
	default:
		writeConfigError(w, http.StatusInternalServerError, "lidarr library request failed", nil)
	}
}

// registerLidarrLibrary wires GET /api/lidarr/artists/{mbid}, GET
// /api/lidarr/add-options and POST /api/lidarr/artists onto mux. Nil-safe:
// when the wiring is absent, every field is nil and every endpoint answers
// 503 rather than panicking.
func registerLidarrLibrary(mux *http.ServeMux, artistStatus LidarrArtistStatusFunc, addOptions LidarrAddOptionsFunc, addArtist LidarrAddArtistFunc) {
	mux.HandleFunc("GET /api/lidarr/artists/{mbid}", func(w http.ResponseWriter, r *http.Request) {
		if artistStatus == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "lidarr library check is not enabled in the configuration", nil)
			return
		}
		got, err := artistStatus(r.Context(), r.PathValue("mbid"))
		if err != nil {
			writeLidarrLibraryError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toLidarrArtistStatusDTO(got))
	})
	mux.HandleFunc("GET /api/lidarr/add-options", func(w http.ResponseWriter, r *http.Request) {
		if addOptions == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "lidarr library check is not enabled in the configuration", nil)
			return
		}
		got, err := addOptions(r.Context())
		if err != nil {
			writeConfigError(w, http.StatusInternalServerError, "failed to load lidarr add options", nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toLidarrAddOptionsDTO(got))
	})
	mux.HandleFunc("POST /api/lidarr/artists", func(w http.ResponseWriter, r *http.Request) {
		if addArtist == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "lidarr library check is not enabled in the configuration", nil)
			return
		}
		var req addArtistRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeConfigError(w, http.StatusBadRequest, "invalid request body", nil)
			return
		}
		params, fieldErrors := validateAddArtistRequest(req)
		if len(fieldErrors) > 0 {
			writeConfigError(w, http.StatusUnprocessableEntity, "validation failed", fieldErrors)
			return
		}

		// The server's WriteTimeout (30s, see cmd/slskdarr/main.go) exists to
		// bound ordinary request handlers, but EnsureArtist can exceed it on a
		// first-time add: a live probe against Lidarr 3.1.0.4875 measured POST
		// /artist alone taking over 30s while it fetched the artist's metadata
		// (see internal/app/lidarr_library.go). Clear the deadline the same way
		// registerStream does for SSE connections, so a slow-but-successful
		// add isn't killed mid-response. If the underlying ResponseWriter
		// doesn't support write deadlines at all, fail cleanly now instead of
		// running the slow call only to have the write silently fail later.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
			writeConfigError(w, http.StatusInternalServerError, "streaming not supported", nil)
			return
		}

		result, err := addArtist(r.Context(), params)
		if err != nil {
			writeLidarrLibraryError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(addArtistResultDTO{
			ArtistID: result.ArtistID, AlreadyInLibrary: result.AlreadyInLibrary,
		})
	})
}
