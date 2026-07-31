// Package observ: identify.go serves issue #321's identify modal: GET
// /api/identify/search runs one combined artist+album MusicBrainz search,
// /albums/{mbid}/editions lists a release-group's editions (each with its
// own track count, never collapsed to a band - see core.MBRelease), and
// /albums/{mbid}/lidarr reports the read-only Lidarr library status. All
// three are nil-safe, mirroring registerSearch/registerShares: when the
// [musicbrainz] config section is absent, every field below is nil and every
// endpoint answers 503 instead of panicking. The MusicBrainz-backed
// endpoints wrap their array in an object carrying a total field, since
// MusicBrainz caps how many rows a single call returns (see
// internal/musicbrainz.Client) - the caller detects truncation by comparing
// total against the array's length.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// IdentifySearchFunc runs a combined artist+album search against
// MusicBrainz (typically backed by app.Identify.SearchReleaseGroups) for GET
// /api/identify/search. artist may be blank; album may not (see
// app.Identify.SearchReleaseGroups). total is MusicBrainz's true match count
// and may exceed len(results) when the result was capped.
type IdentifySearchFunc func(ctx context.Context, artist, album string) (results []core.MBReleaseGroup, total int, err error)

// IdentifyAlbumEditionsFunc lists a release-group's editions (typically
// backed by app.Identify.AlbumEditions) for GET
// /api/identify/albums/{mbid}/editions. total is MusicBrainz's true match
// count and may exceed len(releases) when the result was capped.
type IdentifyAlbumEditionsFunc func(ctx context.Context, releaseGroupMBID string) (releases []core.MBRelease, total int, err error)

// IdentifyAlbumLidarrStatusFunc reports the read-only Lidarr library status
// for a release-group (typically backed by app.Identify.AlbumLidarrStatus)
// for GET /api/identify/albums/{mbid}/lidarr.
type IdentifyAlbumLidarrStatusFunc func(ctx context.Context, releaseGroupMBID string) (app.LidarrAlbumStatus, error)

// mbSearchResultDTO is the JSON shape of one core.MBReleaseGroup as returned
// by GET /api/identify/search - the table row the design's identifyOpen
// block draws (ARTIST / ALBUM · TYPE · YEAR · EDITIONS).
type mbSearchResultDTO struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Artist           string   `json:"artist,omitempty"`
	ArtistID         string   `json:"artistId,omitempty"`
	PrimaryType      string   `json:"primaryType,omitempty"`
	SecondaryTypes   []string `json:"secondaryTypes"`
	FirstReleaseDate string   `json:"firstReleaseDate,omitempty"`
	EditionCount     int      `json:"editionCount"`
	Score            int      `json:"score"`
}

// mbSearchDTO is the JSON shape of GET /api/identify/search. total is
// MusicBrainz's true match count - the caller compares it against
// len(results) to detect truncation, since the slice itself is capped (see
// internal/musicbrainz.Client.SearchReleaseGroups).
type mbSearchDTO struct {
	Results []mbSearchResultDTO `json:"results"`
	Total   int                 `json:"total"`
}

func toMBSearchDTO(groups []core.MBReleaseGroup, total int) mbSearchDTO {
	out := make([]mbSearchResultDTO, len(groups))
	for i, g := range groups {
		secondary := g.SecondaryTypes
		if secondary == nil {
			secondary = make([]string, 0)
		}
		out[i] = mbSearchResultDTO{
			ID: g.ID, Title: g.Title, Artist: g.ArtistName, ArtistID: g.ArtistID,
			PrimaryType: g.PrimaryType, SecondaryTypes: secondary,
			FirstReleaseDate: g.FirstReleaseDate, EditionCount: g.EditionCount, Score: g.Score,
		}
	}
	return mbSearchDTO{Results: out, Total: total}
}

// mbReleaseDTO is the JSON shape of one core.MBRelease - one edition of a
// release-group, with its own track count. trackCount always serialises,
// even as 0, so trackCountKnown stays the sole signal for "unknown" - an
// omitempty on trackCount would make a real 0-media edition ambiguous with
// one MusicBrainz never reported data for (issue #321's explicit warning
// about box-set editions with no media data).
type mbReleaseDTO struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Date            string `json:"date,omitempty"`
	Country         string `json:"country,omitempty"`
	Status          string `json:"status,omitempty"`
	TrackCount      int    `json:"trackCount"`
	TrackCountKnown bool   `json:"trackCountKnown"`
}

// mbReleaseListDTO is the JSON shape of GET
// /api/identify/albums/{mbid}/editions. total is MusicBrainz's true match
// count - the caller compares it against len(editions) to detect
// truncation, since the slice itself is capped (see
// internal/musicbrainz.Client.Releases).
type mbReleaseListDTO struct {
	Editions []mbReleaseDTO `json:"editions"`
	Total    int            `json:"total"`
}

func toMBReleaseListDTO(releases []core.MBRelease, total int) mbReleaseListDTO {
	out := make([]mbReleaseDTO, len(releases))
	for i, r := range releases {
		out[i] = mbReleaseDTO{
			ID: r.ID, Title: r.Title, Date: r.Date, Country: r.Country, Status: r.Status,
			TrackCount: r.TrackCount, TrackCountKnown: r.TrackCountKnown,
		}
	}
	return mbReleaseListDTO{Editions: out, Total: total}
}

// lidarrAlbumStatusDTO is the JSON shape of app.LidarrAlbumStatus. albumId is
// omitted when the album is not in the library (or unknown).
type lidarrAlbumStatusDTO struct {
	Known     bool  `json:"known"`
	InLibrary bool  `json:"inLibrary"`
	AlbumID   int64 `json:"albumId,omitempty"`
}

func toLidarrAlbumStatusDTO(s app.LidarrAlbumStatus) lidarrAlbumStatusDTO {
	return lidarrAlbumStatusDTO{Known: s.Known, InLibrary: s.InLibrary, AlbumID: s.AlbumID}
}

// writeIdentifyError maps an Identify service error to a status code and
// writes it: errors.Is(err, app.ErrIdentifyQueryInvalid) -> 422,
// errors.Is(err, app.ErrIdentifyUnavailable) -> 503, anything else -> 500.
func writeIdentifyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrIdentifyQueryInvalid):
		writeConfigError(w, http.StatusUnprocessableEntity, "validation failed",
			map[string]string{"query": "is required"})
	case errors.Is(err, app.ErrIdentifyUnavailable):
		writeConfigError(w, http.StatusServiceUnavailable, err.Error(), nil)
	default:
		writeConfigError(w, http.StatusInternalServerError, "identify request failed", nil)
	}
}

// registerIdentify wires GET /api/identify/search,
// /api/identify/albums/{mbid}/editions and /api/identify/albums/{mbid}/lidarr
// onto mux. Nil-safe: when the [musicbrainz] config section is absent, every
// field is nil and every endpoint answers 503 rather than panicking.
func registerIdentify(mux *http.ServeMux, search IdentifySearchFunc, albumEditions IdentifyAlbumEditionsFunc, albumLidarr IdentifyAlbumLidarrStatusFunc) {
	mux.HandleFunc("GET /api/identify/search", func(w http.ResponseWriter, r *http.Request) {
		if search == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "identify is not enabled in the configuration", nil)
			return
		}
		got, total, err := search(r.Context(), r.URL.Query().Get("artist"), r.URL.Query().Get("album"))
		if err != nil {
			writeIdentifyError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toMBSearchDTO(got, total))
	})
	mux.HandleFunc("GET /api/identify/albums/{mbid}/editions", func(w http.ResponseWriter, r *http.Request) {
		if albumEditions == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "identify is not enabled in the configuration", nil)
			return
		}
		got, total, err := albumEditions(r.Context(), r.PathValue("mbid"))
		if err != nil {
			writeIdentifyError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toMBReleaseListDTO(got, total))
	})
	mux.HandleFunc("GET /api/identify/albums/{mbid}/lidarr", func(w http.ResponseWriter, r *http.Request) {
		if albumLidarr == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "identify is not enabled in the configuration", nil)
			return
		}
		got, err := albumLidarr(r.Context(), r.PathValue("mbid"))
		if err != nil {
			writeIdentifyError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toLidarrAlbumStatusDTO(got))
	})
}
