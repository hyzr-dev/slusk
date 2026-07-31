package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

func newIdentifyTestHandler(reg *prometheus.Registry, artists IdentifyArtistsFunc, artistAlbums IdentifyArtistAlbumsFunc, albumEditions IdentifyAlbumEditionsFunc, albumLidarr IdentifyAlbumLidarrStatusFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.IdentifyArtists = artists
	deps.IdentifyArtistAlbums = artistAlbums
	deps.IdentifyAlbumEditions = albumEditions
	deps.IdentifyAlbumLidarrStatus = albumLidarr
	return NewServer(deps)
}

func TestIdentifyEndpointsNilDepsAnswer503(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newIdentifyTestHandler(reg, nil, nil, nil, nil)

	for _, path := range []string{
		"/api/identify/artists?query=metallica",
		"/api/identify/artists/a1/albums",
		"/api/identify/albums/rg1/editions",
		"/api/identify/albums/rg1/lidarr",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, rec.Code)
		}
	}
}

func TestIdentifyArtistsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	artists := func(ctx context.Context, query string) ([]core.MBArtist, int, error) {
		switch query {
		case "metallica":
			return []core.MBArtist{{ID: "a1", Name: "Metallica", Score: 100}}, 1, nil
		case "":
			return nil, 0, app.ErrIdentifyQueryInvalid
		case "down":
			return nil, 0, app.ErrIdentifyUnavailable
		default:
			return nil, 0, errBoom
		}
	}
	h := newIdentifyTestHandler(reg, artists, nil, nil, nil)

	t.Run("success", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/artists?query=metallica", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var got mbArtistSearchDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Artists) != 1 || got.Artists[0].ID != "a1" || got.Artists[0].Score != 100 || got.Total != 1 {
			t.Fatalf("unexpected body: %+v", got)
		}
	})

	t.Run("blank query is 422", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/artists", nil))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	t.Run("musicbrainz unavailable is 503", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/artists?query=down", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})

	t.Run("unmapped error is 500", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/artists?query=boom", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestIdentifyArtistAlbumsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	artistAlbums := func(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, int, error) {
		if artistMBID != "a1" {
			t.Errorf("mbid = %q, want a1", artistMBID)
		}
		return []core.MBReleaseGroup{{ID: "rg1", Title: "Ride the Lightning"}}, 1, nil
	}
	h := newIdentifyTestHandler(reg, nil, artistAlbums, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/artists/a1/albums", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got mbReleaseGroupListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Albums) != 1 || got.Albums[0].ID != "rg1" || got.Total != 1 {
		t.Fatalf("unexpected body: %+v", got)
	}
}

// TestIdentifyAlbumEditionsEndpointDoesNotCollapseToBand covers issue #321's
// explicit requirement: the API must expose every edition's own track count,
// never a min/max band.
func TestIdentifyAlbumEditionsEndpointDoesNotCollapseToBand(t *testing.T) {
	reg := prometheus.NewRegistry()
	albumEditions := func(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error) {
		return []core.MBRelease{
			{ID: "r1", TrackCount: 8, TrackCountKnown: true},
			{ID: "r2", TrackCount: 97, TrackCountKnown: true},
			{ID: "r3"},
		}, 3, nil
	}
	h := newIdentifyTestHandler(reg, nil, nil, albumEditions, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/albums/rg1/editions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got mbReleaseListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Editions) != 3 {
		t.Fatalf("expected all 3 per-edition entries, got %d", len(got.Editions))
	}
	if got.Editions[0].TrackCount != 8 || got.Editions[1].TrackCount != 97 {
		t.Fatalf("editions must carry their own track counts, not a collapsed band: %+v", got.Editions)
	}
	if got.Editions[2].TrackCountKnown {
		t.Fatalf("an edition with no media data must not be marked as a known track count: %+v", got.Editions[2])
	}
}

func TestIdentifyAlbumLidarrStatusEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	albumLidarr := func(ctx context.Context, releaseGroupMBID string) (app.LidarrAlbumStatus, error) {
		switch releaseGroupMBID {
		case "in-library":
			return app.LidarrAlbumStatus{Known: true, InLibrary: true, AlbumID: 42}, nil
		case "unknown":
			return app.LidarrAlbumStatus{Known: false}, nil
		default:
			return app.LidarrAlbumStatus{Known: true, InLibrary: false}, nil
		}
	}
	h := newIdentifyTestHandler(reg, nil, nil, nil, albumLidarr)

	t.Run("in library", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/albums/in-library/lidarr", nil))
		var got lidarrAlbumStatusDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Known || !got.InLibrary || got.AlbumID != 42 {
			t.Fatalf("unexpected body: %+v", got)
		}
	})

	t.Run("unknown never claims absence", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identify/albums/unknown/lidarr", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (unknown is a normal outcome, not an error)", rec.Code)
		}
		var got lidarrAlbumStatusDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Known {
			t.Fatalf("expected known = false, got %+v", got)
		}
	})
}
