package observ

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

func newLidarrLibraryTestHandler(reg *prometheus.Registry, artistStatus LidarrArtistStatusFunc, addOptions LidarrAddOptionsFunc, addArtist LidarrAddArtistFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.LidarrArtistStatus = artistStatus
	deps.LidarrAddOptions = addOptions
	deps.LidarrAddArtist = addArtist
	return NewServer(deps)
}

// postLidarrArtistsRecorder wraps httptest.ResponseRecorder to also satisfy
// the SetWriteDeadline hook http.ResponseController looks for - a bare
// httptest.ResponseRecorder doesn't implement it, and the POST
// /api/lidarr/artists handler must clear the write deadline to survive past
// the server's WriteTimeout (see registerLidarrLibrary's doc comment on that
// call). Exercising the handler against a plain httptest.NewRecorder() would
// always 500 with "streaming not supported" once that call is reached.
// Mirrors testStreamRecorder in stream_test.go.
type postLidarrArtistsRecorder struct {
	*httptest.ResponseRecorder
}

func (postLidarrArtistsRecorder) SetWriteDeadline(time.Time) error { return nil }

func newPostLidarrArtistsRecorder() postLidarrArtistsRecorder {
	return postLidarrArtistsRecorder{httptest.NewRecorder()}
}

func TestLidarrLibraryEndpointsNilDepsAnswer503(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newLidarrLibraryTestHandler(reg, nil, nil, nil)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/lidarr/artists/artist-1", nil),
		httptest.NewRequest(http.MethodGet, "/api/lidarr/add-options", nil),
		httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(`{}`))),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", req.Method, req.URL.Path, rec.Code)
		}
	}
}

func TestLidarrArtistStatusEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	artistStatus := func(ctx context.Context, artistMBID string) (app.LidarrArtistStatus, error) {
		switch artistMBID {
		case "in-library":
			return app.LidarrArtistStatus{Known: true, InLibrary: true, ArtistID: 9, Name: "Aphex Twin"}, nil
		case "unknown":
			return app.LidarrArtistStatus{Known: false}, nil
		default:
			return app.LidarrArtistStatus{Known: true, InLibrary: false}, nil
		}
	}
	h := newLidarrLibraryTestHandler(reg, artistStatus, nil, nil)

	t.Run("in library", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/lidarr/artists/in-library", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var got lidarrArtistStatusDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Known || !got.InLibrary || got.ArtistID != 9 || got.Name != "Aphex Twin" {
			t.Fatalf("unexpected body: %+v", got)
		}
	})

	t.Run("unknown never claims absence", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/lidarr/artists/unknown", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (unknown is a normal outcome, not an error)", rec.Code)
		}
		var got lidarrArtistStatusDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Known {
			t.Fatalf("expected known = false, got %+v", got)
		}
	})
}

func TestLidarrAddOptionsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	addOptions := func(ctx context.Context) (app.LidarrAddOptions, error) {
		return app.LidarrAddOptions{
			RootFolders:      []core.LidarrRootFolder{{ID: 1, Path: "/music/library", DefaultQualityProfileID: 2, DefaultMetadataProfileID: 1}},
			QualityProfiles:  []core.LidarrProfile{{ID: 1, Name: "Any"}, {ID: 2, Name: "Lossless"}},
			MetadataProfiles: []core.LidarrProfile{{ID: 1, Name: "Standard"}},
		}, nil
	}
	h := newLidarrLibraryTestHandler(reg, nil, addOptions, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/lidarr/add-options", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got lidarrAddOptionsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.RootFolders) != 1 || got.RootFolders[0].Path != "/music/library" ||
		len(got.QualityProfiles) != 2 || len(got.MetadataProfiles) != 1 {
		t.Fatalf("unexpected body: %+v", got)
	}
}

func TestPostLidarrArtistsValidation(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		t.Fatal("addArtist must not be called when validation fails")
		return app.AddArtistResult{}, nil
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `not json`},
		{"missing fields", `{}`},
		{"zero quality profile", `{"artistMbid":"a1","artistName":"Aphex Twin","rootFolderPath":"/music","qualityProfileId":0,"metadataProfileId":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(c.body))))
			if c.name == "invalid json" {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPostLidarrArtistsSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		if params.ArtistMBID != "a1" || params.ArtistName != "Aphex Twin" ||
			params.RootFolderPath != "/music" || params.QualityProfileID != 2 || params.MetadataProfileID != 1 {
			t.Fatalf("unexpected params: %+v", params)
		}
		return app.AddArtistResult{ArtistID: 9, AlreadyInLibrary: false}, nil
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	// The body carries neither a monitor choice nor an album id: the add only
	// ensures the artist exists, unmonitored (see internal/app's
	// lidarr_library.go).
	body := validPostLidarrArtistsBody()
	rec := newPostLidarrArtistsRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got addArtistResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ArtistID != 9 || got.AlreadyInLibrary {
		t.Fatalf("unexpected body: %+v", got)
	}
}

// TestPostLidarrArtistsIgnoresAMonitorField guards the removal itself: a
// client still sending the old "monitor"/"albumMbid" fields must not be
// rejected, and must certainly not reach the service carrying a monitoring
// intent - there is nowhere left for one to go.
func TestPostLidarrArtistsIgnoresAMonitorField(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		return app.AddArtistResult{ArtistID: 9}, nil
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	body := `{"artistMbid":"a1","artistName":"Aphex Twin","albumMbid":"rg1","rootFolderPath":"/music","qualityProfileId":2,"metadataProfileId":1,"monitor":"all"}`
	rec := newPostLidarrArtistsRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func validPostLidarrArtistsBody() string {
	return `{"artistMbid":"a1","artistName":"Aphex Twin","rootFolderPath":"/music","qualityProfileId":2,"metadataProfileId":1}`
}

// TestPostLidarrArtistsAddUncertainMaps502 covers the wire contract for
// issue #331's backend review: an error wrapping app.ErrLidarrLibraryAddUncertain
// must produce a distinct 502 with code "addUncertain", not the generic 500
// every other failure gets - a client must not blindly retry this one, since
// the create may already have landed.
func TestPostLidarrArtistsAddUncertainMaps502(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		return app.AddArtistResult{}, fmt.Errorf("lidarr add artist: %w", app.ErrLidarrLibraryAddUncertain)
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	rec := newPostLidarrArtistsRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(validPostLidarrArtistsBody()))))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "addUncertain" {
		t.Fatalf("code = %q, want %q", got.Code, "addUncertain")
	}
}

// TestPostLidarrArtistsInvalidRootFolderMaps422 covers backend review #7:
// app.ErrLidarrLibraryInvalidRootFolder must produce a 422 naming
// rootFolderPath specifically, not the generic "missing or invalid" message
// app.ErrLidarrLibraryQueryInvalid gets.
func TestPostLidarrArtistsInvalidRootFolderMaps422(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		return app.AddArtistResult{}, app.ErrLidarrLibraryInvalidRootFolder
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	rec := newPostLidarrArtistsRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(validPostLidarrArtistsBody()))))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.FieldErrors["rootFolderPath"]; !ok {
		t.Fatalf("fieldErrors = %v, want a rootFolderPath entry", got.FieldErrors)
	}
}

// TestPostLidarrArtistsQueryInvalidDoesNotGuessAField covers backend review
// #8's smaller item: app.ErrLidarrLibraryQueryInvalid covers five different
// possible fields depending on the code path that produced it, so it must no
// longer hardcode a misleading "mbid" field error.
func TestPostLidarrArtistsQueryInvalidDoesNotGuessAField(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		return app.AddArtistResult{}, app.ErrLidarrLibraryQueryInvalid
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	rec := newPostLidarrArtistsRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lidarr/artists", bytes.NewReader([]byte(validPostLidarrArtistsBody()))))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.FieldErrors) != 0 {
		t.Fatalf("fieldErrors = %v, want none - this error can't honestly name a single field", got.FieldErrors)
	}
}

// TestPostLidarrArtistsSurvivesServerWriteTimeout is a regression test for
// issue #331's backend review blocker: cmd/slskdarr/main.go sets the shared
// server's WriteTimeout to 30s, and EnsureArtist can exceed
// that on a first-time add. httptest.NewServer (used by every other test in
// this file) sets no WriteTimeout at all, which is exactly why a live probe
// against this endpoint missed the bug - this test deliberately configures
// one, shorter than addArtist's simulated delay, and would fail with a
// truncated/reset response without registerLidarrLibrary's
// SetWriteDeadline(time.Time{}) call.
func TestPostLidarrArtistsSurvivesServerWriteTimeout(t *testing.T) {
	reg := prometheus.NewRegistry()
	addArtist := func(ctx context.Context, params app.AddArtistParams) (app.AddArtistResult, error) {
		time.Sleep(80 * time.Millisecond) // outlast the server's WriteTimeout below
		return app.AddArtistResult{ArtistID: 9}, nil
	}
	h := newLidarrLibraryTestHandler(reg, nil, nil, addArtist)

	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 20 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/lidarr/artists", "application/json", strings.NewReader(validPostLidarrArtistsBody()))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 - the write deadline must be cleared before the slow call runs", resp.StatusCode)
	}
	var got addArtistResultDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.ArtistID != 9 {
		t.Fatalf("unexpected body: %+v", got)
	}
}
