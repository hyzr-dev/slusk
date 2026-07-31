// Package observ: identify.go serves issue #321's identify modal: GET
// /api/identify/artists searches MusicBrainz, /artists/{mbid}/albums lists
// that artist's release-groups, /albums/{mbid}/editions lists a
// release-group's editions (each with its own track count, never collapsed
// to a band - see core.MBRelease), and /albums/{mbid}/lidarr reports the
// read-only Lidarr library status. All four are nil-safe, mirroring
// registerSearch/registerShares: when the [musicbrainz] config section is
// absent, every field below is nil and every endpoint answers 503 instead of
// panicking. The three MusicBrainz-backed endpoints wrap their array in an
// object carrying a total field, since MusicBrainz caps how many rows a
// single call returns (see internal/musicbrainz.Client) - the caller detects
// truncation by comparing total against the array's length.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// IdentifyArtistsFunc searches MusicBrainz artists (typically backed by
// app.Identify.SearchArtists) for GET /api/identify/artists. total is
// MusicBrainz's true match count and may exceed len(artists) when the
// result was capped.
type IdentifyArtistsFunc func(ctx context.Context, query string) (artists []core.MBArtist, total int, err error)

// IdentifyArtistAlbumsFunc lists an artist's release-groups (typically backed
// by app.Identify.ArtistAlbums) for GET /api/identify/artists/{mbid}/albums.
// total is MusicBrainz's true match count and may exceed len(groups) when
// the result was capped.
type IdentifyArtistAlbumsFunc func(ctx context.Context, artistMBID string) (groups []core.MBReleaseGroup, total int, err error)

// IdentifyAlbumEditionsFunc lists a release-group's editions (typically
// backed by app.Identify.AlbumEditions) for GET
// /api/identify/albums/{mbid}/editions. total is MusicBrainz's true match
// count and may exceed len(releases) when the result was capped.
type IdentifyAlbumEditionsFunc func(ctx context.Context, releaseGroupMBID string) (releases []core.MBRelease, total int, err error)

// IdentifyAlbumLidarrStatusFunc reports the read-only Lidarr library status
// for a release-group (typically backed by app.Identify.AlbumLidarrStatus)
// for GET /api/identify/albums/{mbid}/lidarr.
type IdentifyAlbumLidarrStatusFunc func(ctx context.Context, releaseGroupMBID string) (app.LidarrAlbumStatus, error)

// mbArtistDTO is the JSON shape of one core.MBArtist.
type mbArtistDTO struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type,omitempty"`
	Country        string `json:"country,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	Score          int    `json:"score"`
}

// mbArtistSearchDTO is the JSON shape of GET /api/identify/artists. total is
// MusicBrainz's true match count - the caller compares it against
// len(artists) to detect truncation, since the slice itself is capped (see
// internal/musicbrainz.Client.SearchArtists).
type mbArtistSearchDTO struct {
	Artists []mbArtistDTO `json:"artists"`
	Total   int           `json:"total"`
}

func toMBArtistSearchDTO(artists []core.MBArtist, total int) mbArtistSearchDTO {
	out := make([]mbArtistDTO, len(artists))
	for i, a := range artists {
		out[i] = mbArtistDTO{
			ID: a.ID, Name: a.Name, Type: a.Type,
			Country: a.Country, Disambiguation: a.Disambiguation, Score: a.Score,
		}
	}
	return mbArtistSearchDTO{Artists: out, Total: total}
}

// mbReleaseGroupDTO is the JSON shape of one core.MBReleaseGroup.
type mbReleaseGroupDTO struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	FirstReleaseDate string   `json:"firstReleaseDate,omitempty"`
	PrimaryType      string   `json:"primaryType,omitempty"`
	SecondaryTypes   []string `json:"secondaryTypes"`
}

// mbReleaseGroupListDTO is the JSON shape of GET
// /api/identify/artists/{mbid}/albums. total is MusicBrainz's true match
// count - the caller compares it against len(albums) to detect truncation,
// since the slice itself is capped (see
// internal/musicbrainz.Client.ReleaseGroups).
type mbReleaseGroupListDTO struct {
	Albums []mbReleaseGroupDTO `json:"albums"`
	Total  int                 `json:"total"`
}

func toMBReleaseGroupListDTO(groups []core.MBReleaseGroup, total int) mbReleaseGroupListDTO {
	out := make([]mbReleaseGroupDTO, len(groups))
	for i, g := range groups {
		secondary := g.SecondaryTypes
		if secondary == nil {
			secondary = make([]string, 0)
		}
		out[i] = mbReleaseGroupDTO{
			ID: g.ID, Title: g.Title, FirstReleaseDate: g.FirstReleaseDate,
			PrimaryType: g.PrimaryType, SecondaryTypes: secondary,
		}
	}
	return mbReleaseGroupListDTO{Albums: out, Total: total}
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

// registerIdentify wires GET /api/identify/artists,
// /api/identify/artists/{mbid}/albums, /api/identify/albums/{mbid}/editions
// and /api/identify/albums/{mbid}/lidarr onto mux. Nil-safe: when the
// [musicbrainz] config section is absent, every field is nil and every
// endpoint answers 503 rather than panicking.
func registerIdentify(mux *http.ServeMux, artists IdentifyArtistsFunc, artistAlbums IdentifyArtistAlbumsFunc, albumEditions IdentifyAlbumEditionsFunc, albumLidarr IdentifyAlbumLidarrStatusFunc) {
	mux.HandleFunc("GET /api/identify/artists", func(w http.ResponseWriter, r *http.Request) {
		if artists == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "identify is not enabled in the configuration", nil)
			return
		}
		got, total, err := artists(r.Context(), r.URL.Query().Get("query"))
		if err != nil {
			writeIdentifyError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toMBArtistSearchDTO(got, total))
	})
	mux.HandleFunc("GET /api/identify/artists/{mbid}/albums", func(w http.ResponseWriter, r *http.Request) {
		if artistAlbums == nil {
			writeConfigError(w, http.StatusServiceUnavailable, "identify is not enabled in the configuration", nil)
			return
		}
		got, total, err := artistAlbums(r.Context(), r.PathValue("mbid"))
		if err != nil {
			writeIdentifyError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toMBReleaseGroupListDTO(got, total))
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
