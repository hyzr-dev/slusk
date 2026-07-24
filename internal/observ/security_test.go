package observ

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

const testAuthToken = "correct-horse-battery-staple"

func newSecuredTestHandler(t *testing.T, cancel CancelFunc) http.Handler {
	t.Helper()
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	status := func(context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(context.Context) ([]core.JobView, error) { return nil, nil }
	if cancel == nil {
		cancel = func(context.Context, int64) error { return nil }
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)
	return ProtectPrivateEndpoints(h, NewTokenAuthenticator(testAuthToken))
}

func TestPrivateEndpointsRequireAuthentication(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	for _, path := range []string{"/", "/jobs", "/status", "/api/jobs", "/metrics"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestPprofEndpointsRequireAuthentication(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	for _, path := range []string{
		"/debug/pprof",
		"/debug/pprof/",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/symbol",
		"/debug/pprof/trace",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAnonymousNonGETPprofRequestRequiresAuthentication(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	for _, path := range []string{"/debug/pprof", "/debug/pprof/profile"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAuthenticatedPprofHandlersRejectNonGETMethods(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "root", method: http.MethodPost, path: "/debug/pprof"},
		{name: "index", method: http.MethodPost, path: "/debug/pprof/"},
		{name: "index subtree", method: http.MethodPut, path: "/debug/pprof/goroutine?debug=1"},
		{name: "cmdline", method: http.MethodPost, path: "/debug/pprof/cmdline"},
		{name: "profile capture", method: http.MethodPut, path: "/debug/pprof/profile"},
		{name: "symbol", method: http.MethodPost, path: "/debug/pprof/symbol"},
		{name: "trace capture", method: http.MethodPut, path: "/debug/pprof/trace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+testAuthToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405; body = %s", rec.Code, rec.Body.String())
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Fatalf("Allow = %q, want %q", allow, "GET, HEAD")
			}
		})
	}
}

func TestAuthenticatedPprofRootRedirectsToCanonicalPath(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/debug/pprof", nil)
			req.Header.Set("Authorization", "Bearer "+testAuthToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want 307; body = %s", rec.Code, rec.Body.String())
			}
			if location := rec.Header().Get("Location"); location != "/debug/pprof/" {
				t.Fatalf("Location = %q, want %q", location, "/debug/pprof/")
			}
		})
	}
}

func TestAuthenticatedPprofIndexAllowsHEAD(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodHead, "/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAuthenticatedPprofHandlersResolve(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	tests := []struct {
		name        string
		path        string
		contentType string
		body        string
	}{
		{name: "index", path: "/debug/pprof/", contentType: "text/html", body: "<title>/debug/pprof/</title>"},
		{name: "index subtree", path: "/debug/pprof/goroutine?debug=1", contentType: "text/plain", body: "goroutine profile:"},
		{name: "cmdline", path: "/debug/pprof/cmdline", contentType: "text/plain"},
		{name: "symbol", path: "/debug/pprof/symbol", contentType: "text/plain", body: "num_symbols:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+testAuthToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tt.contentType) {
				t.Fatalf("Content-Type = %q, want it to contain %q", got, tt.contentType)
			}
			if body := rec.Body.String(); tt.body != "" && !strings.Contains(body, tt.body) {
				t.Fatalf("body does not contain %q: %q", tt.body, body)
			}
		})
	}
}

func TestPrivateEndpointsAcceptBearerAndBasicToken(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	tests := []struct {
		name string
		auth func(*http.Request)
	}{
		{name: "bearer", auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testAuthToken) }},
		{name: "basic browser", auth: func(r *http.Request) { r.SetBasicAuth("slskdarr", testAuthToken) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			tt.auth(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPrivateEndpointsRejectUnauthorizedAndMalformedCredentials(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	for _, authorization := range []string{
		"Bearer wrong-token",
		"Bearer",
		"Bearer ",
		"Bearer token with spaces",
		"Token " + testAuthToken,
		"Basic !!!not-base64!!!",
	} {
		t.Run(authorization, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.Header.Set("Authorization", authorization)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestDeleteMutationAuthenticationAndSameOriginProtection(t *testing.T) {
	calls := 0
	del := func(context.Context, int64) error {
		calls++
		return nil
	}
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	status := func(context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(context.Context, int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, del)
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
		{name: "basic malformed origin", auth: "basic", origin: "not a URL", wantStatus: http.StatusForbidden},
		{name: "basic cross origin", auth: "basic", origin: "http://evil.example", wantStatus: http.StatusForbidden},
		{name: "bearer cross origin", auth: "bearer", origin: "http://evil.example", wantStatus: http.StatusForbidden},
		{name: "basic same origin", auth: "basic", origin: "http://example.com", wantStatus: http.StatusNoContent, wantCalls: 1},
		{name: "bearer cli without origin", auth: "bearer", wantStatus: http.StatusNoContent, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls = 0
			req := httptest.NewRequest(http.MethodDelete, "http://example.com/api/jobs/42", nil)
			switch tt.auth {
			case "basic":
				req.SetBasicAuth("slskdarr", testAuthToken)
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
				t.Fatalf("mutation calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestHealthzRemainsPublicAndMinimal(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("body = %q, want empty minimal response", body)
	}
}

func TestReadyzRemainsPublic(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (readiness probe must not require auth)", rec.Code)
	}
}

func TestMutationAuthenticationAndSameOriginProtection(t *testing.T) {
	calls := 0
	cancel := func(context.Context, int64) error {
		calls++
		return nil
	}
	h := newSecuredTestHandler(t, cancel)

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
		{name: "basic malformed origin", auth: "basic", origin: "not a URL", wantStatus: http.StatusForbidden},
		{name: "basic cross origin", auth: "basic", origin: "http://evil.example", wantStatus: http.StatusForbidden},
		{name: "bearer cross origin", auth: "bearer", origin: "http://evil.example", wantStatus: http.StatusForbidden},
		{name: "basic same origin", auth: "basic", origin: "http://example.com", wantStatus: http.StatusNoContent, wantCalls: 1},
		{name: "bearer cli without origin", auth: "bearer", wantStatus: http.StatusNoContent, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls = 0
			req := httptest.NewRequest(http.MethodPost, "http://example.com/api/jobs/42/cancel", strings.NewReader(""))
			switch tt.auth {
			case "basic":
				req.SetBasicAuth("slskdarr", testAuthToken)
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
				t.Fatalf("mutation calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}
