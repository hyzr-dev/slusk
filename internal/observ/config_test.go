package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigHandlerNeverLeaksTheAPIKey(t *testing.T) {
	cfg := AppConfig{
		LidarrURL:          "http://lidarr:8686",
		lidarrAPIKey:       "super-secret-value",
		WantedSyncInterval: "5m",
		MaxActive:          3,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return cfg }).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatal("response body leaked the Lidarr API key")
	}

	var got struct {
		LidarrURL              string `json:"lidarrUrl"`
		LidarrAPIKeyConfigured bool   `json:"lidarrApiKeyConfigured"`
		MaxActive              int    `json:"maxActive"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.LidarrAPIKeyConfigured {
		t.Error("lidarrApiKeyConfigured = false, want true")
	}
	if got.LidarrURL != "http://lidarr:8686" {
		t.Errorf("lidarrUrl = %q", got.LidarrURL)
	}
}

func TestConfigHandlerReportsMissingAPIKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return AppConfig{} }).ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `"lidarrApiKeyConfigured":true`) {
		t.Error("reported a configured key when none was set")
	}
}

func TestConfigHandlerReportsSoulseekEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return AppConfig{SoulseekEnabled: true} }).ServeHTTP(rec, req)

	var got struct {
		SoulseekEnabled bool `json:"soulseekEnabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.SoulseekEnabled {
		t.Error("soulseekEnabled = false, want true")
	}
}

// decodeTestResult reads a connection-test endpoint's {ok,error} response.
func decodeTestResult(t *testing.T, body []byte) connectionTestResult {
	t.Helper()
	var got connectionTestResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestConnectionTestReportsSuccess(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/test/lidarr", nil)

	newConnectionTestHandler("lidarr", func(ctx context.Context) error { return nil }).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeTestResult(t, rec.Body.Bytes())
	if !got.OK || got.Error != "" {
		t.Errorf("result = %+v, want ok with no error", got)
	}
}

func TestConnectionTestReportsProbeError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/test/lidarr", nil)

	probe := func(ctx context.Context) error { return errors.New("lidarr rejected the API key (status 401)") }
	newConnectionTestHandler("lidarr", probe).ServeHTTP(rec, req)

	got := decodeTestResult(t, rec.Body.Bytes())
	if got.OK || !strings.Contains(got.Error, "API key") {
		t.Errorf("result = %+v, want failure carrying the probe's message", got)
	}
}

func TestConnectionTestNilProbeReportsNotEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/test/soulseek", nil)

	newConnectionTestHandler("soulseek", nil).ServeHTTP(rec, req)

	got := decodeTestResult(t, rec.Body.Bytes())
	if got.OK || !strings.Contains(got.Error, "not enabled") {
		t.Errorf("result = %+v, want a not-enabled failure", got)
	}
}

func TestConnectionTestRejectsNonPost(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config/test/lidarr", nil)

	called := false
	probe := func(ctx context.Context) error { called = true; return nil }
	newConnectionTestHandler("lidarr", probe).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if called {
		t.Error("probe ran for a non-POST request")
	}
}
