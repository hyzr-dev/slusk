package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

	newConfigHandler(func() AppConfig { return cfg }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

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

	newConfigHandler(func() AppConfig { return AppConfig{} }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `"lidarrApiKeyConfigured":true`) {
		t.Error("reported a configured key when none was set")
	}
}

func TestConfigHandlerReportsSoulseekEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return AppConfig{SoulseekEnabled: true} }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

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

func TestConfigHandlerGETIncludesWritableSettingsFields(t *testing.T) {
	cfg := AppConfig{MinBitrate: 256, StallTimeout: "10m", Writable: true}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return cfg }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

	var got struct {
		MinBitrate   int    `json:"minBitrate"`
		StallTimeout string `json:"stallTimeout"`
		Writable     bool   `json:"writable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MinBitrate != 256 || got.StallTimeout != "10m" || !got.Writable {
		t.Errorf("got = %+v, want {256 10m true}", got)
	}
}

const validConfigUpdateBody = `{"lidarrUrl":"http://lidarr:8686","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`

func TestConfigHandlerPOSTAppliesValidUpdateAndSchedulesRestart(t *testing.T) {
	var got ConfigUpdate
	writeCalled := make(chan struct{}, 1)
	writer := func(u ConfigUpdate) error {
		got = u
		writeCalled <- struct{}{}
		return nil
	}
	restarted := make(chan struct{}, 1)
	restart := func() { restarted <- struct{}{} }

	body := `{"lidarrUrl":"http://lidarr:8686","lidarrApiKey":"secret-key","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))

	newConfigHandler(func() AppConfig { return AppConfig{} }, writer, restart).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK         bool `json:"ok"`
		Restarting bool `json:"restarting"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.Restarting {
		t.Errorf("response = %+v, want {true true}", resp)
	}

	select {
	case <-writeCalled:
	case <-time.After(time.Second):
		t.Fatal("writer was not called")
	}
	if got.LidarrURL != "http://lidarr:8686" || got.LidarrAPIKey == nil || *got.LidarrAPIKey != "secret-key" ||
		got.WantedSyncInterval != 15*time.Minute || got.StallTimeout != 5*time.Minute || got.MaxActive != 10 || got.MinBitrate != 192 {
		t.Errorf("writer received = %+v", got)
	}

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not called")
	}
	if strings.Contains(rec.Body.String(), "secret-key") {
		t.Error("response body leaked the submitted API key")
	}
}

func TestConfigHandlerPOSTKeepsAPIKeyWhenAbsentOrBlank(t *testing.T) {
	bodies := []string{
		`{"lidarrUrl":"http://lidarr:8686","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`,
		`{"lidarrUrl":"http://lidarr:8686","lidarrApiKey":"","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`,
		`{"lidarrUrl":"http://lidarr:8686","lidarrApiKey":"   ","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`,
	}
	for _, body := range bodies {
		var got ConfigUpdate
		writer := func(u ConfigUpdate) error { got = u; return nil }
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))

		newConfigHandler(func() AppConfig { return AppConfig{} }, writer, func() {}).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("body %q: status = %d, resp = %s", body, rec.Code, rec.Body.String())
		}
		if got.LidarrAPIKey != nil {
			t.Errorf("body %q: LidarrAPIKey = %q, want nil (keep)", body, *got.LidarrAPIKey)
		}
	}
}

func TestConfigHandlerPOSTValidationFailureReportsFieldErrors(t *testing.T) {
	writerCalled, restartCalled := false, false
	writer := func(ConfigUpdate) error { writerCalled = true; return nil }
	restart := func() { restartCalled = true }

	body := `{"lidarrUrl":"not-a-url","wantedSyncInterval":"0s","stallTimeout":"-5m","maxActive":0,"minBitrate":0}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))

	newConfigHandler(func() AppConfig { return AppConfig{} }, writer, restart).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"lidarrUrl", "wantedSyncInterval", "stallTimeout", "maxActive", "minBitrate"} {
		if resp.FieldErrors[field] == "" {
			t.Errorf("missing fieldErrors[%q]: %+v", field, resp.FieldErrors)
		}
	}
	if writerCalled || restartCalled {
		t.Error("writer or restart ran despite a validation failure")
	}
}

func TestConfigHandlerPOSTNotWritableReports409(t *testing.T) {
	restartCalled := false
	writer := func(ConfigUpdate) error { return ErrConfigNotWritable }
	restart := func() { restartCalled = true }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(validConfigUpdateBody))

	newConfigHandler(func() AppConfig { return AppConfig{} }, writer, restart).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error, "mount") {
		t.Errorf("error = %q, want a mount hint", resp.Error)
	}
	if restartCalled {
		t.Error("restart ran despite a write failure")
	}
}

func TestConfigHandlerPOSTMalformedBodyReports400(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader("not json"))

	newConfigHandler(func() AppConfig { return AppConfig{} }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestConfigHandlerRejectsUnsupportedMethods(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/config", nil)

		newConfigHandler(func() AppConfig { return AppConfig{} }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}
}

func TestConfigHandlerPOSTNeverLeaksSubmittedAPIKeyOnFailure(t *testing.T) {
	const secret = "super-secret-value"

	// 422: an otherwise-invalid request that still carries the secret key.
	rec422 := httptest.NewRecorder()
	req422 := httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"lidarrUrl":"not-a-url","lidarrApiKey":"`+secret+`","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`))
	newConfigHandler(func() AppConfig { return AppConfig{} }, noopConfigWriter, noopRestart).ServeHTTP(rec422, req422)
	if rec422.Code != http.StatusUnprocessableEntity {
		t.Fatalf("422 case: status = %d, body = %s", rec422.Code, rec422.Body.String())
	}
	if strings.Contains(rec422.Body.String(), secret) {
		t.Error("422 response leaked the submitted API key")
	}

	// 500: a validly-shaped request whose writer fails for an unrelated reason.
	rec500 := httptest.NewRecorder()
	req500 := httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"lidarrUrl":"http://lidarr:8686","lidarrApiKey":"`+secret+`","wantedSyncInterval":"15m","stallTimeout":"5m","maxActive":10,"minBitrate":192}`))
	failingWriter := func(ConfigUpdate) error { return errors.New("disk full: " + secret) }
	newConfigHandler(func() AppConfig { return AppConfig{} }, failingWriter, noopRestart).ServeHTTP(rec500, req500)
	if rec500.Code != http.StatusInternalServerError {
		t.Fatalf("500 case: status = %d, body = %s", rec500.Code, rec500.Body.String())
	}
	if strings.Contains(rec500.Body.String(), secret) {
		t.Error("500 response leaked the submitted API key")
	}
}
