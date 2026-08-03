package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/app"
	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/prometheus/client_golang/prometheus"
)

func newSearchTestHandler(reg *prometheus.Registry, start StartSearchFunc, snapshot SearchSnapshotFunc, stop StopSearchFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.StartSearch = start
	deps.SearchSnapshot = snapshot
	deps.StopSearch = stop
	return NewServer(deps)
}

var testSearchStartedAt = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func testSearchSession() core.SearchSession {
	return core.SearchSession{
		ID: "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4", Query: "in rainbows", StartedAt: testSearchStartedAt,
		Done: false, Streaming: true, Total: 1,
		Groups: []core.SearchGroup{{
			ID: "a1b2c3d4a1b2c3d4", Peer: "lossless_lars", Folder: `@@abc\Music\Radiohead\In Rainbows`,
			Title: "In Rainbows", Parent: "Radiohead", TrackCount: 1, SizeBytes: 34112000,
			Format: "flac", BitRate: 1010, SampleRate: 44100, BitDepth: 16, Score: 0.91,
			Files: []core.SearchFile{{
				Filename: `@@abc\Music\Radiohead\In Rainbows\05 - Nude.flac`, Name: "05 - Nude.flac",
				Size: 34112000, BitRate: 1010, Duration: 253, SampleRate: 44100, BitDepth: 16,
			}},
		}},
	}
}

// TestSearchEndpointsStatusTable exercises POST/GET/DELETE /api/search
// against every documented outcome (issue #58's endpoint table).
func TestSearchEndpointsStatusTable(t *testing.T) {
	reg := prometheus.NewRegistry()
	sess := testSearchSession()

	start := func(ctx context.Context, query string) (core.SearchSession, error) {
		switch query {
		case "in rainbows":
			return sess, nil
		case "":
			return core.SearchSession{}, app.ErrSearchQueryInvalid
		case "busy":
			return core.SearchSession{}, app.ErrSearchBusy
		case "unavailable":
			return core.SearchSession{}, app.ErrSearchUnavailable
		default:
			return core.SearchSession{}, errBoom
		}
	}
	snapshot := func(id string) (core.SearchSession, bool) {
		if id == sess.ID {
			return sess, true
		}
		return core.SearchSession{}, false
	}
	stop := func(id string) bool { return id == sess.ID }

	h := newSearchTestHandler(reg, start, snapshot, stop)

	t.Run("POST malformed body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader("{not json"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("POST blank query", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":""}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		var body errorResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.FieldErrors["query"] == "" {
			t.Fatalf("expected a query field error, got %+v", body)
		}
	})

	t.Run("POST busy", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"busy"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "30" {
			t.Fatalf("Retry-After = %q, want 30", got)
		}
	})

	t.Run("POST unavailable", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"unavailable"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if rec.Header().Get("Retry-After") != "" {
			t.Fatalf("unexpected Retry-After on ErrSearchUnavailable")
		}
	})

	t.Run("POST other error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"boom"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("POST success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"in rainbows"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
		}
		var got searchSessionDTO
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != sess.ID || got.Query != sess.Query || !got.Streaming || got.Total != 1 {
			t.Fatalf("session DTO = %+v", got)
		}
		if len(got.Groups) != 1 || got.Groups[0].Peer != "lossless_lars" || got.Groups[0].Files[0].Filename != sess.Groups[0].Files[0].Filename {
			t.Fatalf("group DTO = %+v", got.Groups)
		}
	})

	t.Run("GET found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search/"+sess.ID, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GET not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search/unknown00unknown00unknown00unkn", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("DELETE found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/search/"+sess.ID, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	})

	t.Run("DELETE not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/search/unknown00unknown00unknown00unkn", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

// errBoom is a generic non-sentinel error for the "anything else -> 500" case.
var errBoom = &testError{"boom"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }

// TestSearchEndpointsNilDepsAnswerServiceUnavailable mirrors
// TestSharesEndpointNilSharesReportsDisabled's nil-safety, but as 503s since
// POST/GET/DELETE /api/search are action endpoints with nothing to report
// when no peer backend is wired.
func TestSearchEndpointsNilDepsAnswerServiceUnavailable(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newSearchTestHandler(reg, nil, nil, nil)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"q"}`)),
		httptest.NewRequest(http.MethodGet, "/api/search/anyidatall00000000000000000000", nil),
		httptest.NewRequest(http.MethodDelete, "/api/search/anyidatall00000000000000000000", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d, want 503", req.Method, req.URL.Path, rec.Code)
		}
	}
}

// TestSearchMutationAuthenticationAndSameOriginProtection mirrors
// TestDeleteMutationAuthenticationAndSameOriginProtection for POST
// /api/search: ProtectPrivateEndpoints' same-origin-mutation rule applies to
// any POST/DELETE regardless of path.
func TestSearchMutationAuthenticationAndSameOriginProtection(t *testing.T) {
	calls := 0
	start := func(ctx context.Context, query string) (core.SearchSession, error) {
		calls++
		return core.SearchSession{ID: "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"}, nil
	}
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	deps := testServerDeps(reg)
	deps.StartSearch = start
	h := NewServer(deps)
	h = ProtectPrivateEndpoints(h, NewTokenAuthenticator(testAuthToken))

	tests := []struct {
		name       string
		auth       string
		origin     string
		wantStatus int
		wantCalls  int
	}{
		{name: "anonymous", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", auth: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "basic without origin", auth: "basic", wantStatus: http.StatusForbidden},
		{name: "basic cross origin", auth: "basic", origin: "http://evil.example", wantStatus: http.StatusForbidden},
		{name: "basic same origin", auth: "basic", origin: "http://example.com", wantStatus: http.StatusCreated, wantCalls: 1},
		{name: "bearer cli without origin", auth: "bearer", wantStatus: http.StatusCreated, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls = 0
			req := httptest.NewRequest(http.MethodPost, "http://example.com/api/search", strings.NewReader(`{"query":"q"}`))
			switch tt.auth {
			case "basic":
				req.SetBasicAuth("slusk", testAuthToken)
			case "bearer":
				req.Header.Set("Authorization", "Bearer "+testAuthToken)
			default:
				if tt.auth != "" {
					req.Header.Set("Authorization", tt.auth)
				}
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if calls != tt.wantCalls {
				t.Fatalf("start calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}
