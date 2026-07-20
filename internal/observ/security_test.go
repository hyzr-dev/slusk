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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig)
	return ProtectPrivateEndpoints(h, NewTokenAuthenticator(testAuthToken))
}

func TestPrivateEndpointsRequireAuthentication(t *testing.T) {
	h := newSecuredTestHandler(t, nil)
	for _, path := range []string{"/", "/dashboard.js", "/status", "/api/jobs", "/metrics"} {
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
