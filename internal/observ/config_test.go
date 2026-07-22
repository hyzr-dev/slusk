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

func strPtr(s string) *string { return &s }

// fullAppConfig returns an AppConfig with every field populated and every
// ...Configured boolean true, for GET-shape assertions.
func fullAppConfig() AppConfig {
	return AppConfig{
		Lidarr: LidarrView{URL: "http://lidarr:8686", APIKeyConfigured: true},
		Slskd:  SlskdView{URL: "http://slskd:5030", APIKeyConfigured: true},
		Pipeline: PipelineView{
			Backend: "slskd", MaxCandidatesPerAlbum: 5, MaxActive: 30, MaxRetries: 10,
			MaxInflightPerPeer: 3, MaxTransferRetries: 3, MinBitrate: 192,
			TransferDeadline: "1h0m0s", StallTimeout: "5m0s", SearchTimeout: "45s",
			BackoffBase: "15m0s", BackoffCap: "24h0m0s", CandidateTTL: "24h0m0s",
			FailedReviveAfter: "720h0m0s", StuckAfter: "1h0m0s", TickTimeout: "5m0s",
			ImportConfirmTimeout: "3m0s", WantedSyncInterval: "15m0s",
			DiscoveryInterval: "30s", SelectingInterval: "10s", DownloadingInterval: "15s",
			ImportingInterval: "30s", ManualImportTimeout: "10m0s", ImportRetryCooldown: "5m0s",
			Weights: WeightsView{Format: 1, Bitrate: 1, Reliability: 1, FileCount: 1, KnownUser: 1},
		},
		Soulseek: SoulseekView{
			Enabled: true, ServerAddress: "server.slsknet.org:2242", Username: "souluser",
			PasswordConfigured: true, ListenAddr: "0.0.0.0:2234", UploadSlots: 2,
			Gluetun:       GluetunView{ControlURL: "http://127.0.0.1:8000", APIKeyConfigured: true},
			SharedFolders: []SharedFolderView{{Name: "Music", Path: "/shares/music"}},
		},
		Store:    StoreView{DSNConfigured: true},
		Observ:   ObservView{ListenAddr: ":9090", AuthTokenConfigured: true, LogLevel: "info"},
		Paths:    PathsView{SlskdCompleteDir: "/music/slskd-downloads"},
		Writable: true,
	}
}

// secretSentinels are values that must never appear anywhere in a response
// body, used across the GET shape test and every POST failure-path test.
var secretSentinels = []string{
	"lidarr-secret-value", "slskd-secret-value", "soulseek-secret-value",
	"gluetun-secret-value", "dsn-secret-value", "observ-secret-value",
}

func assertNoSecretLeak(t *testing.T, body string) {
	t.Helper()
	for _, secret := range secretSentinels {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaked a secret value %q:\n%s", secret, body)
		}
	}
}

func TestConfigHandlerGETReturnsFullNestedShapeWithNoSecrets(t *testing.T) {
	cfg := fullAppConfig()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return cfg }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertNoSecretLeak(t, rec.Body.String())

	var got AppConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Lidarr.URL != cfg.Lidarr.URL || !got.Lidarr.APIKeyConfigured {
		t.Errorf("Lidarr = %+v", got.Lidarr)
	}
	if !got.Slskd.APIKeyConfigured {
		t.Errorf("Slskd = %+v", got.Slskd)
	}
	if got.Pipeline.MaxActive != 30 || got.Pipeline.Weights.KnownUser != 1 || got.Pipeline.CandidateTTL != "24h0m0s" {
		t.Errorf("Pipeline = %+v", got.Pipeline)
	}
	if !got.Soulseek.Enabled || !got.Soulseek.PasswordConfigured || !got.Soulseek.Gluetun.APIKeyConfigured {
		t.Errorf("Soulseek = %+v", got.Soulseek)
	}
	if len(got.Soulseek.SharedFolders) != 1 || got.Soulseek.SharedFolders[0].Name != "Music" {
		t.Errorf("Soulseek.SharedFolders = %+v", got.Soulseek.SharedFolders)
	}
	if !got.Store.DSNConfigured {
		t.Errorf("Store = %+v", got.Store)
	}
	if !got.Observ.AuthTokenConfigured || got.Observ.LogLevel != "info" {
		t.Errorf("Observ = %+v", got.Observ)
	}
	if got.Paths.SlskdCompleteDir != "/music/slskd-downloads" || !got.Writable {
		t.Errorf("Paths = %+v, Writable = %v", got.Paths, got.Writable)
	}
}

func TestConfigHandlerGETReportsUnconfiguredSecretsAndDisabledSoulseek(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return AppConfig{} }, noopConfigWriter, noopRestart).ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, configured := range []string{
		`"apiKeyConfigured":true`, `"passwordConfigured":true`, `"dsnConfigured":true`, `"authTokenConfigured":true`,
	} {
		if strings.Contains(body, configured) {
			t.Errorf("reported %q on a zero-value config", configured)
		}
	}
	var got AppConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Soulseek.Enabled {
		t.Error("Soulseek.Enabled = true on a zero-value config")
	}
}

// --- POST happy path ---

func validFullRequest() configUpdateRequest {
	return configUpdateRequest{
		Lidarr: lidarrUpdateRequest{URL: "http://lidarr:8686", APIKey: strPtr("lidarr-secret-value")},
		Slskd:  slskdUpdateRequest{URL: "http://slskd:5030", APIKey: strPtr("slskd-secret-value")},
		Pipeline: pipelineUpdateRequest{
			Backend: "slskd", MaxCandidatesPerAlbum: 5, MaxActive: 30, MaxRetries: 10,
			MaxInflightPerPeer: 3, MaxTransferRetries: 3, MinBitrate: 192,
			TransferDeadline: "1h0m0s", StallTimeout: "5m0s", SearchTimeout: "45s",
			BackoffBase: "15m0s", BackoffCap: "24h0m0s", CandidateTTL: "24h0m0s",
			FailedReviveAfter: "720h0m0s", StuckAfter: "1h0m0s", TickTimeout: "5m0s",
			ImportConfirmTimeout: "3m0s", WantedSyncInterval: "15m0s",
			DiscoveryInterval: "30s", SelectingInterval: "10s", DownloadingInterval: "15s",
			ImportingInterval: "30s", ManualImportTimeout: "10m0s", ImportRetryCooldown: "5m0s",
			Weights: weightsUpdateRequest{Format: 1, Bitrate: 1, Reliability: 1, FileCount: 1, KnownUser: 0.6},
		},
		Soulseek: soulseekUpdateRequest{
			ServerAddress: "server.slsknet.org:2242",
			Username:      "souluser",
			Password:      strPtr("soulseek-secret-value"),
			ListenAddr:    "0.0.0.0:2234",
			UploadSlots:   2,
			Gluetun: gluetunUpdateRequest{
				ControlURL: "http://127.0.0.1:8000",
				APIKey:     strPtr("gluetun-secret-value"),
			},
			SharedFolders: []sharedFolderUpdateRequest{{Name: "Music", Path: "/shares/music"}},
		},
		Store: storeUpdateRequest{DSN: strPtr("dsn-secret-value")},
		Observ: observUpdateRequest{
			ListenAddr: "127.0.0.1:9090",
			AuthToken:  strPtr("observ-secret-value"),
			LogLevel:   "warn",
		},
		Paths: pathsUpdateRequest{SlskdCompleteDir: "/music/downloads"},
	}
}

func postConfig(t *testing.T, writer ConfigWriter, restart func(), body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(raw)))
	newConfigHandler(func() AppConfig { return AppConfig{} }, writer, restart).ServeHTTP(rec, req)
	return rec
}

func TestConfigHandlerPOSTAppliesValidUpdateAcrossEverySection(t *testing.T) {
	var got ConfigUpdate
	writeCalled := make(chan struct{}, 1)
	writer := func(u ConfigUpdate) error {
		got = u
		writeCalled <- struct{}{}
		return nil
	}
	restarted := make(chan struct{}, 1)
	restart := func() { restarted <- struct{}{} }

	rec := postConfig(t, writer, restart, validFullRequest())
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
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not called")
	}

	if got.Lidarr.URL != "http://lidarr:8686" || got.Lidarr.APIKey == nil || *got.Lidarr.APIKey != "lidarr-secret-value" {
		t.Errorf("Lidarr = %+v", got.Lidarr)
	}
	if got.Slskd.APIKey == nil || *got.Slskd.APIKey != "slskd-secret-value" {
		t.Errorf("Slskd = %+v", got.Slskd)
	}
	if got.Pipeline.MaxActive != 30 || got.Pipeline.Weights.KnownUser != 0.6 || got.Pipeline.CandidateTTL != 24*time.Hour {
		t.Errorf("Pipeline = %+v", got.Pipeline)
	}
	if got.Soulseek.ServerAddress != "server.slsknet.org:2242" || got.Soulseek.Password == nil || *got.Soulseek.Password != "soulseek-secret-value" {
		t.Errorf("Soulseek = %+v", got.Soulseek)
	}
	if got.Soulseek.Gluetun.APIKey == nil || *got.Soulseek.Gluetun.APIKey != "gluetun-secret-value" {
		t.Errorf("Soulseek.Gluetun = %+v", got.Soulseek.Gluetun)
	}
	if len(got.Soulseek.SharedFolders) != 1 || got.Soulseek.SharedFolders[0].Name != "Music" {
		t.Errorf("Soulseek.SharedFolders = %+v", got.Soulseek.SharedFolders)
	}
	if got.Store.DSN == nil || *got.Store.DSN != "dsn-secret-value" {
		t.Errorf("Store = %+v", got.Store)
	}
	if got.Observ.AuthToken == nil || *got.Observ.AuthToken != "observ-secret-value" || got.Observ.LogLevel != "warn" {
		t.Errorf("Observ = %+v", got.Observ)
	}
	if got.Paths.SlskdCompleteDir != "/music/downloads" {
		t.Errorf("Paths = %+v", got.Paths)
	}
	if strings.Contains(rec.Body.String(), "-secret-value") {
		t.Error("response body leaked a submitted secret")
	}
}

func TestConfigHandlerPOSTKeepsAllSixSecretsWhenAbsentOrBlank(t *testing.T) {
	base := validFullRequest()
	blank := "   "
	variants := []struct {
		name string
		body configUpdateRequest
	}{
		{"absent", func() configUpdateRequest {
			r := base
			r.Lidarr.APIKey, r.Slskd.APIKey, r.Soulseek.Password, r.Soulseek.Gluetun.APIKey, r.Store.DSN, r.Observ.AuthToken = nil, nil, nil, nil, nil, nil
			return r
		}()},
		{"blank", func() configUpdateRequest {
			r := base
			r.Lidarr.APIKey, r.Slskd.APIKey, r.Soulseek.Password, r.Soulseek.Gluetun.APIKey, r.Store.DSN, r.Observ.AuthToken =
				strPtr(""), strPtr(""), strPtr(""), strPtr(""), strPtr(""), strPtr("")
			return r
		}()},
		{"whitespace", func() configUpdateRequest {
			r := base
			r.Lidarr.APIKey, r.Slskd.APIKey, r.Soulseek.Password, r.Soulseek.Gluetun.APIKey, r.Store.DSN, r.Observ.AuthToken =
				&blank, &blank, &blank, &blank, &blank, &blank
			return r
		}()},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			var got ConfigUpdate
			writer := func(u ConfigUpdate) error { got = u; return nil }
			rec := postConfig(t, writer, func() {}, v.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got.Lidarr.APIKey != nil {
				t.Errorf("Lidarr.APIKey = %q, want nil (keep)", *got.Lidarr.APIKey)
			}
			if got.Slskd.APIKey != nil {
				t.Errorf("Slskd.APIKey = %q, want nil (keep)", *got.Slskd.APIKey)
			}
			if got.Soulseek.Password != nil {
				t.Errorf("Soulseek.Password = %q, want nil (keep)", *got.Soulseek.Password)
			}
			if got.Soulseek.Gluetun.APIKey != nil {
				t.Errorf("Soulseek.Gluetun.APIKey = %q, want nil (keep)", *got.Soulseek.Gluetun.APIKey)
			}
			if got.Store.DSN != nil {
				t.Errorf("Store.DSN = %q, want nil (keep)", *got.Store.DSN)
			}
			if got.Observ.AuthToken != nil {
				t.Errorf("Observ.AuthToken = %q, want nil (keep)", *got.Observ.AuthToken)
			}
		})
	}
}

// --- per-field validation, nested dotted keys ---

func TestConfigHandlerPOSTValidationFailureReportsNestedFieldErrors(t *testing.T) {
	req := validFullRequest()
	req.Lidarr.URL = "not-a-url"
	req.Pipeline.Backend = "bogus"
	req.Pipeline.MaxActive = 0
	req.Pipeline.Weights.KnownUser = -1
	req.Soulseek.ListenAddr = "not-a-valid-addr"
	req.Soulseek.Gluetun.ControlURL = "not a url"
	req.Soulseek.SharedFolders = []sharedFolderUpdateRequest{{Name: "bad/name", Path: "relative/path"}}
	req.Observ.ListenAddr = ""
	req.Observ.LogLevel = "verbose"
	req.Paths.SlskdCompleteDir = ""

	writerCalled, restartCalled := false, false
	writer := func(ConfigUpdate) error { writerCalled = true; return nil }
	restart := func() { restartCalled = true }

	rec := postConfig(t, writer, restart, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"lidarr.url",
		"pipeline.backend",
		"pipeline.maxActive",
		"pipeline.weights.knownUser",
		"soulseek.listenAddr",
		"soulseek.gluetun.controlUrl",
		"soulseek.sharedFolders[0].name",
		"soulseek.sharedFolders[0].path",
		"observ.listenAddr",
		"observ.logLevel",
		"paths.slskdCompleteDir",
	} {
		if resp.FieldErrors[key] == "" {
			t.Errorf("missing fieldErrors[%q]: %+v", key, resp.FieldErrors)
		}
	}
	if writerCalled || restartCalled {
		t.Error("writer or restart ran despite a per-field validation failure")
	}
}

func TestConfigHandlerPOSTAllowsBlankSlskdURLWhenBackendIsSoulseek(t *testing.T) {
	// slskd.url's presence is a cross-field rule (only required when
	// backend=slskd); the per-field validator must not reject a blank value.
	req := validFullRequest()
	req.Pipeline.Backend = "soulseek"
	req.Slskd.URL = ""

	var got ConfigUpdate
	writer := func(u ConfigUpdate) error { got = u; return nil }
	rec := postConfig(t, writer, func() {}, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.Slskd.URL != "" {
		t.Errorf("Slskd.URL = %q, want blank", got.Slskd.URL)
	}
}

// --- backstop (cross-field) validation failures surface as 422 with empty fieldErrors ---

func TestConfigHandlerPOSTBackstopValidationReports422WithEmptyFieldErrors(t *testing.T) {
	const message = "rendered config failed validation, nothing was written: invalid config: pipeline.backend must be \"slskd\" or \"soulseek\""
	writer := func(ConfigUpdate) error { return &ConfigValidationError{Message: message} }
	restartCalled := false
	restart := func() { restartCalled = true }

	rec := postConfig(t, writer, restart, validFullRequest())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != message {
		t.Errorf("error = %q, want %q", resp.Error, message)
	}
	if len(resp.FieldErrors) != 0 {
		t.Errorf("fieldErrors = %+v, want empty", resp.FieldErrors)
	}
	if restartCalled {
		t.Error("restart ran despite a backstop validation failure")
	}
}

func TestConfigHandlerPOSTNotWritableReports409(t *testing.T) {
	restartCalled := false
	writer := func(ConfigUpdate) error { return ErrConfigNotWritable }
	restart := func() { restartCalled = true }

	rec := postConfig(t, writer, restart, validFullRequest())
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

func TestConfigHandlerPOSTGenericWriterFailureReports500WithoutEchoingError(t *testing.T) {
	writer := func(ConfigUpdate) error { return errors.New("disk full: /some/internal/path") }
	rec := postConfig(t, writer, func() {}, validFullRequest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "disk full") || strings.Contains(rec.Body.String(), "/some/internal/path") {
		t.Errorf("500 response echoed the underlying error: %s", rec.Body.String())
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

// --- secret non-leak sweep across success/422/500 ---

func TestConfigHandlerPOSTNeverLeaksAnySecretOnAnyPath(t *testing.T) {
	// success
	okWriter := func(ConfigUpdate) error { return nil }
	rec := postConfig(t, okWriter, func() {}, validFullRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("success case: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertNoSecretLeak(t, rec.Body.String())

	// 422: per-field validation failure, secrets still present in the request
	invalid := validFullRequest()
	invalid.Lidarr.URL = "not-a-url"
	rec422 := postConfig(t, noopConfigWriter, noopRestart, invalid)
	if rec422.Code != http.StatusUnprocessableEntity {
		t.Fatalf("422 case: status = %d, body = %s", rec422.Code, rec422.Body.String())
	}
	assertNoSecretLeak(t, rec422.Body.String())

	// 422: backstop failure
	backstopWriter := func(ConfigUpdate) error { return &ConfigValidationError{Message: "invalid config: something"} }
	rec422b := postConfig(t, backstopWriter, func() {}, validFullRequest())
	if rec422b.Code != http.StatusUnprocessableEntity {
		t.Fatalf("backstop 422 case: status = %d, body = %s", rec422b.Code, rec422b.Body.String())
	}
	assertNoSecretLeak(t, rec422b.Body.String())

	// 500: writer fails for an unrelated reason
	failingWriter := func(ConfigUpdate) error { return errors.New("boom: lidarr-secret-value") }
	rec500 := postConfig(t, failingWriter, func() {}, validFullRequest())
	if rec500.Code != http.StatusInternalServerError {
		t.Fatalf("500 case: status = %d, body = %s", rec500.Code, rec500.Body.String())
	}
	assertNoSecretLeak(t, rec500.Body.String())
}

// --- connection tests: unchanged behavior ---

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
