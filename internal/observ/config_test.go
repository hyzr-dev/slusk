package observ

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigHandlerNeverLeaksTheAPIKey(t *testing.T) {
	cfg := AppConfig{
		LidarrURL:              "http://lidarr:8686",
		lidarrAPIKey:           "super-secret-value",
		ReconcileInterval:      "5m",
		MaxConcurrentDownloads: 3,
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
		MaxConcurrentDownloads int    `json:"maxConcurrentDownloads"`
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
