package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservSlogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":      slog.LevelInfo,
		"info":  slog.LevelInfo,
		"DEBUG": slog.LevelDebug,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for value, want := range cases {
		if got := (ObservConfig{LogLevel: value}).SlogLevel(); got != want {
			t.Errorf("SlogLevel(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestLoadInvalidLogLevelFails(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Replace(string(base), "[observ]\n", "[observ]\nlog_level = \"bogus\"\n", 1)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected error for an invalid observ.log_level, got nil")
	}
	if !strings.Contains(err.Error(), "log_level") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadSoulseekSharesAndUploadSlots(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := string(base) + `
[soulseek]
username = "me"
password = "secret"
upload_slots = 3
allow_private_peer_addresses = true
[[soulseek.shared_folders]]
name = "Music"
path = "/shares/music"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Soulseek.UploadSlots != 3 || len(cfg.Soulseek.SharedFolders) != 1 || cfg.Soulseek.SharedFolders[0].Name != "Music" {
		t.Fatalf("Soulseek config = %+v", cfg.Soulseek)
	}
	if !cfg.Soulseek.AllowPrivatePeerAddresses {
		t.Fatalf("AllowPrivatePeerAddresses = false, want true")
	}

	for _, invalid := range []string{
		"upload_slots = 0\n",
		"[[soulseek.shared_folders]]\nname = \"../secret\"\npath = \"/shares/music\"\n",
		"[[soulseek.shared_folders]]\nname = \"Music\"\npath = \"relative\"\n",
		"[[soulseek.shared_folders]]\nname = \" Music\"\npath = \"/shares/music\"\n",
		"[[soulseek.shared_folders]]\nname = \"Music\"\npath = \"/shares/music \"\n",
	} {
		bad := filepath.Join(t.TempDir(), "config.toml")
		body := string(base) + "\n[soulseek]\nusername = \"me\"\npassword = \"secret\"\n" + invalid
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(bad); err == nil {
			t.Fatalf("Load accepted invalid soulseek config: %s", invalid)
		}
	}
}

func TestSoulseekShareValidationRejectsDuplicates(t *testing.T) {
	cfg := Config{Soulseek: SoulseekConfig{
		Username: "me", Password: "secret", ServerAddress: defaultSoulseekServerAddress,
		ListenAddr: defaultSoulseekListenAddr, UploadSlots: 2,
		SharedFolders: []SharedFolderConfig{{Name: "Music", Path: "/one"}, {Name: "music", Path: "/one"}},
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "shared_folders") {
		t.Fatalf("Validate duplicate shares = %v", err)
	}
}

func TestLoadSoulseekDefaultsTwoUploadSlots(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, append(base, []byte("\n[soulseek]\nusername=\"me\"\npassword=\"secret\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Soulseek.UploadSlots != 2 {
		t.Fatalf("UploadSlots = %d", cfg.Soulseek.UploadSlots)
	}
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load valid: %v", err)
	}
	if cfg.Lidarr.URL != "http://lidarr:8686" {
		t.Errorf("Lidarr.URL = %q", cfg.Lidarr.URL)
	}
	if cfg.Pipeline.MaxCandidatesPerAlbum != 5 {
		t.Errorf("MaxCandidatesPerAlbum = %d", cfg.Pipeline.MaxCandidatesPerAlbum)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	_, err := Load("testdata/unknown_key.toml")
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention unknown key: %v", err)
	}
}

func TestLoadRejectsMissingDuration(t *testing.T) {
	_, err := Load("testdata/missing_duration.toml")
	if err == nil {
		t.Fatal("expected error for missing pipeline.search_timeout, got nil")
	}
	if !strings.Contains(err.Error(), "search_timeout") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

func TestLoadValidHasPipelineFields(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.MinBitrate != 192 {
		t.Errorf("MinBitrate = %d", cfg.Pipeline.MinBitrate)
	}
	if cfg.Pipeline.SearchTimeout.Duration.Seconds() != 30 {
		t.Errorf("SearchTimeout = %v", cfg.Pipeline.SearchTimeout.Duration)
	}
	if cfg.Paths.SlskdCompleteDir != "/music/slskd-downloads" {
		t.Errorf("SlskdCompleteDir = %q", cfg.Paths.SlskdCompleteDir)
	}
}

func TestLoadAbsentPipelineSectionYieldsDefaults(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Pipeline
	if p.MaxActive != 30 {
		t.Errorf("MaxActive = %d, want 30", p.MaxActive)
	}
	if p.MaxRetries != 10 {
		t.Errorf("MaxRetries = %d, want 10", p.MaxRetries)
	}
	if p.BackoffBase.Duration != 15*time.Minute {
		t.Errorf("BackoffBase = %v, want 15m", p.BackoffBase.Duration)
	}
	if p.BackoffCap.Duration != 24*time.Hour {
		t.Errorf("BackoffCap = %v, want 24h", p.BackoffCap.Duration)
	}
	if p.CandidateTTL.Duration != 24*time.Hour {
		t.Errorf("CandidateTTL = %v, want 24h", p.CandidateTTL.Duration)
	}
	if p.FailedReviveAfter.Duration != 720*time.Hour {
		t.Errorf("FailedReviveAfter = %v, want 720h", p.FailedReviveAfter.Duration)
	}
	if p.StuckAfter.Duration != time.Hour {
		t.Errorf("StuckAfter = %v, want 1h", p.StuckAfter.Duration)
	}
	if p.TickTimeout.Duration != 5*time.Minute {
		t.Errorf("TickTimeout = %v, want 5m", p.TickTimeout.Duration)
	}
	if p.ImportConfirmTimeout.Duration != 3*time.Minute {
		t.Errorf("ImportConfirmTimeout = %v, want 3m", p.ImportConfirmTimeout.Duration)
	}
	if p.WantedSyncInterval.Duration != 15*time.Minute {
		t.Errorf("WantedSyncInterval = %v, want 15m", p.WantedSyncInterval.Duration)
	}
	if p.DiscoveryInterval.Duration != 30*time.Second {
		t.Errorf("DiscoveryInterval = %v, want 30s", p.DiscoveryInterval.Duration)
	}
	if p.SelectingInterval.Duration != 10*time.Second {
		t.Errorf("SelectingInterval = %v, want 10s", p.SelectingInterval.Duration)
	}
	if p.DownloadingInterval.Duration != 15*time.Second {
		t.Errorf("DownloadingInterval = %v, want 15s", p.DownloadingInterval.Duration)
	}
	if p.ImportingInterval.Duration != 30*time.Second {
		t.Errorf("ImportingInterval = %v, want 30s", p.ImportingInterval.Duration)
	}
	if p.ManualImportTimeout.Duration != 10*time.Minute {
		t.Errorf("ManualImportTimeout = %v, want 10m", p.ManualImportTimeout.Duration)
	}
	if p.ImportRetryCooldown.Duration != 5*time.Minute {
		t.Errorf("ImportRetryCooldown = %v, want 5m", p.ImportRetryCooldown.Duration)
	}
}

func TestLoadPipelineOverrides(t *testing.T) {
	cfg, err := Load("testdata/pipeline_overrides.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Pipeline
	if p.MaxActive != 50 {
		t.Errorf("MaxActive = %d, want 50", p.MaxActive)
	}
	if p.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", p.MaxRetries)
	}
	if p.BackoffBase.Duration != 10*time.Minute {
		t.Errorf("BackoffBase = %v, want 10m", p.BackoffBase.Duration)
	}
	if p.BackoffCap.Duration != 12*time.Hour {
		t.Errorf("BackoffCap = %v, want 12h", p.BackoffCap.Duration)
	}
	if p.CandidateTTL.Duration != 12*time.Hour {
		t.Errorf("CandidateTTL = %v, want 12h", p.CandidateTTL.Duration)
	}
	if p.FailedReviveAfter.Duration != 360*time.Hour {
		t.Errorf("FailedReviveAfter = %v, want 360h", p.FailedReviveAfter.Duration)
	}
	if p.StuckAfter.Duration != 30*time.Minute {
		t.Errorf("StuckAfter = %v, want 30m", p.StuckAfter.Duration)
	}
	if p.TickTimeout.Duration != 2*time.Minute {
		t.Errorf("TickTimeout = %v, want 2m", p.TickTimeout.Duration)
	}
	if p.ImportConfirmTimeout.Duration != 4*time.Minute {
		t.Errorf("ImportConfirmTimeout = %v, want 4m", p.ImportConfirmTimeout.Duration)
	}
	if p.WantedSyncInterval.Duration != 10*time.Minute {
		t.Errorf("WantedSyncInterval = %v, want 10m", p.WantedSyncInterval.Duration)
	}
	if p.DiscoveryInterval.Duration != 20*time.Second {
		t.Errorf("DiscoveryInterval = %v, want 20s", p.DiscoveryInterval.Duration)
	}
	if p.SelectingInterval.Duration != 5*time.Second {
		t.Errorf("SelectingInterval = %v, want 5s", p.SelectingInterval.Duration)
	}
	if p.DownloadingInterval.Duration != 10*time.Second {
		t.Errorf("DownloadingInterval = %v, want 10s", p.DownloadingInterval.Duration)
	}
	if p.ImportingInterval.Duration != 20*time.Second {
		t.Errorf("ImportingInterval = %v, want 20s", p.ImportingInterval.Duration)
	}
	if p.ManualImportTimeout.Duration != 8*time.Minute {
		t.Errorf("ManualImportTimeout = %v, want 8m", p.ManualImportTimeout.Duration)
	}
	if p.ImportRetryCooldown.Duration != 2*time.Minute {
		t.Errorf("ImportRetryCooldown = %v, want 2m", p.ImportRetryCooldown.Duration)
	}
}

func TestLoadRejectsInvalidPipelineValue(t *testing.T) {
	_, err := Load("testdata/pipeline_invalid.toml")
	if err == nil {
		t.Fatal("expected error for invalid pipeline.max_active, got nil")
	}
	if !strings.Contains(err.Error(), "pipeline.max_active") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadRejectsUnknownPipelineKey(t *testing.T) {
	_, err := Load("testdata/pipeline_unknown_key.toml")
	if err == nil {
		t.Fatal("expected error for unknown key in [pipeline], got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention unknown key: %v", err)
	}
}

func TestObservAuthPolicy(t *testing.T) {
	base, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tests := []struct {
		name       string
		listenAddr string
		token      string
		wantError  string
	}{
		// Issue #279: form-based session login is now mandatory browser-side
		// auth regardless of observ.listen_addr, so an absent token no longer
		// makes a non-loopback listener a validation error - it just means no
		// machine/API credential is accepted alongside the browser login.
		{name: "IPv4 loopback without token", listenAddr: "127.0.0.1:9090"},
		{name: "IPv6 loopback without token", listenAddr: "[::1]:9090"},
		{name: "localhost without token", listenAddr: "localhost:9090"},
		{name: "wildcard with token", listenAddr: "0.0.0.0:9090", token: "a-secret-token"},
		{name: "wildcard without token", listenAddr: "0.0.0.0:9090"},
		{name: "repository placeholder token", listenAddr: "0.0.0.0:9090", token: "REPLACE_WITH_A_LONG_RANDOM_TOKEN", wantError: "must be replaced with a generated token"},
		{name: "token with whitespace", listenAddr: "0.0.0.0:9090", token: "not a bearer token", wantError: "must not contain whitespace"},
		{name: "empty host without token", listenAddr: ":9090"},
		{name: "LAN address without token", listenAddr: "192.168.1.20:9090"},
		{name: "malformed listener", listenAddr: "0.0.0.0", token: "a-secret-token", wantError: "valid host:port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Observ.ListenAddr = tt.listenAddr
			cfg.Observ.AuthToken = tt.token
			err := cfg.Validate()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want text %q", err, tt.wantError)
			}
		})
	}
}

func TestValidationErrorDoesNotExposeObservToken(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const secret = "never-log-this-observ-token"
	cfg.Observ.AuthToken = secret
	cfg.Observ.ListenAddr = "malformed"
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error exposed auth token: %v", err)
	}
}

func TestLoadWithoutSoulseekSectionLeavesItDisabled(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Soulseek.Enabled() {
		t.Fatalf("Soulseek.Enabled() = true, want false for a config with no [soulseek] section")
	}
}

func TestLoadSoulseekValid(t *testing.T) {
	cfg, err := Load("testdata/soulseek_valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Soulseek.Enabled() {
		t.Fatal("Soulseek.Enabled() = false, want true")
	}
	if cfg.Soulseek.Username != "souluser" {
		t.Errorf("Username = %q", cfg.Soulseek.Username)
	}
	if cfg.Soulseek.Password != "soulpass" {
		t.Errorf("Password = %q", cfg.Soulseek.Password)
	}
	if cfg.Soulseek.ServerAddress != "server.slsknet.org:2242" {
		t.Errorf("ServerAddress = %q, want the default", cfg.Soulseek.ServerAddress)
	}
	if cfg.Soulseek.ListenAddr != "0.0.0.0:2234" {
		t.Errorf("ListenAddr = %q, want the default", cfg.Soulseek.ListenAddr)
	}
	if cfg.Soulseek.UploadSlots != 4 || len(cfg.Soulseek.SharedFolders) != 1 || cfg.Soulseek.SharedFolders[0] != (SharedFolderConfig{Name: "Music", Path: "/shares/music"}) {
		t.Errorf("sharing config = slots %d, folders %+v", cfg.Soulseek.UploadSlots, cfg.Soulseek.SharedFolders)
	}
	if cfg.Soulseek.Gluetun != (GluetunConfig{}) {
		t.Errorf("Gluetun = %+v, want the zero value when [soulseek.gluetun] is absent", cfg.Soulseek.Gluetun)
	}
}

func TestLoadSoulseekGluetunValid(t *testing.T) {
	cfg, err := Load("testdata/soulseek_gluetun_valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Soulseek.Gluetun.ControlURL != "http://127.0.0.1:8000" {
		t.Errorf("Gluetun.ControlURL = %q", cfg.Soulseek.Gluetun.ControlURL)
	}
	if cfg.Soulseek.Gluetun.APIKey != "gluetun-key" {
		t.Errorf("Gluetun.APIKey = %q", cfg.Soulseek.Gluetun.APIKey)
	}
}

func TestLoadSoulseekGluetunInvalidURL(t *testing.T) {
	_, err := Load("testdata/soulseek_gluetun_invalid_url.toml")
	if err == nil {
		t.Fatal("expected error for an invalid soulseek.gluetun.control_url, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek.gluetun.control_url") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadSoulseekGluetunAPIKeyWithoutURL(t *testing.T) {
	_, err := Load("testdata/soulseek_gluetun_api_key_without_url.toml")
	if err == nil {
		t.Fatal("expected error for soulseek.gluetun.api_key without control_url, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek.gluetun.api_key") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadSoulseekGluetunUnknownKeyFails(t *testing.T) {
	_, err := Load("testdata/soulseek_gluetun_unknown_key.toml")
	if err == nil {
		t.Fatal("expected error for an unknown key in [soulseek.gluetun], got nil")
	}
	if !strings.Contains(err.Error(), "controll_url") {
		t.Errorf("error = %q, want it to name the unknown key controll_url", err.Error())
	}
}

func TestLoadSoulseekMissingPassword(t *testing.T) {
	_, err := Load("testdata/soulseek_missing_password.toml")
	if err == nil {
		t.Fatal("expected error for missing soulseek.password, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek.password") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

func TestLoadSoulseekInvalidListenAddr(t *testing.T) {
	_, err := Load("testdata/soulseek_invalid_listen_addr.toml")
	if err == nil {
		t.Fatal("expected error for an invalid soulseek.listen_addr, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek.listen_addr") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadSoulseekZeroPortListenAddr(t *testing.T) {
	_, err := Load("testdata/soulseek_zero_port_listen_addr.toml")
	if err == nil {
		t.Fatal("expected error for a zero-port soulseek.listen_addr, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek.listen_addr") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadSoulseekNonNumericPortListenAddr(t *testing.T) {
	// net.SplitHostPort only validates the host:port shape, not that the
	// port is actually numeric; "0.0.0.0:abc" must still be rejected here
	// rather than only failing later, at bind time.
	_, err := Load("testdata/soulseek_non_numeric_port_listen_addr.toml")
	if err == nil {
		t.Fatal("expected error for a non-numeric-port soulseek.listen_addr, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek.listen_addr") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadDefaultBackendIsSlskd(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Backend != BackendSlskd {
		t.Errorf("Backend = %q, want %q", cfg.Pipeline.Backend, BackendSlskd)
	}
}

func TestLoadBackendSoulseekWithoutSoulseekSectionFails(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Replace(string(base), "[pipeline]\n", "[pipeline]\nbackend = \"soulseek\"\n", 1)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected error for backend = soulseek without a [soulseek] section, got nil")
	}
	if !strings.Contains(err.Error(), "soulseek") {
		t.Errorf("error should mention the missing soulseek section: %v", err)
	}
}

func TestLoadBackendSoulseekWithSlskdSectionValid(t *testing.T) {
	// An [slskd] section alongside backend = "soulseek" is valid: the slskd
	// config is simply unused by the pipeline.
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Replace(string(base), "[pipeline]\n", "[pipeline]\nbackend = \"soulseek\"\n", 1)
	contents += "\n[soulseek]\nusername = \"souluser\"\npassword = \"soulpass\"\n"
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Backend != BackendSoulseek {
		t.Errorf("Backend = %q, want %q", cfg.Pipeline.Backend, BackendSoulseek)
	}
}

func TestLoadBackendSoulseekWithoutSlskdSectionValid(t *testing.T) {
	const contents = `
[lidarr]
url = "http://lidarr:8686"
api_key = "abc"

[pipeline]
backend = "soulseek"
max_candidates_per_album = 5
transfer_deadline = "30m"
stall_timeout = "5m"
search_timeout = "30s"
min_bitrate = 192
max_inflight_per_peer = 3
max_transfer_retries = 3

[pipeline.weights]
format = 1.0
bitrate = 0.5
reliability = 0.8
file_count = 1.0

[store]
dsn = "postgres://slskdarr:password@postgres:5432/slskdarr?sslmode=disable"

[observ]
listen_addr = "127.0.0.1:9090"

[paths]
slskd_complete_dir = "/music/slskd-downloads"

[soulseek]
username = "souluser"
password = "soulpass"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Backend != BackendSoulseek {
		t.Errorf("Backend = %q, want %q", cfg.Pipeline.Backend, BackendSoulseek)
	}
}

func TestLoadUnknownBackendValueFails(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Replace(string(base), "[pipeline]\n", "[pipeline]\nbackend = \"bogus\"\n", 1)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected error for an unknown pipeline.backend value, got nil")
	}
	if !strings.Contains(err.Error(), "pipeline.backend") {
		t.Errorf("error should name the invalid field: %v", err)
	}
}

func TestLoadWithoutMusicBrainzSectionLeavesItDisabled(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MusicBrainz.Enabled() {
		t.Fatal("MusicBrainz.Enabled() = true, want false for a config with no [musicbrainz] section")
	}
}

func TestLoadMusicBrainzValidAppliesDefaults(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := string(base) + "\n[musicbrainz]\ncontact = \"me@example.com\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MusicBrainz.Enabled() {
		t.Fatal("MusicBrainz.Enabled() = false, want true")
	}
	if cfg.MusicBrainz.Contact != "me@example.com" {
		t.Errorf("Contact = %q", cfg.MusicBrainz.Contact)
	}
	if cfg.MusicBrainz.BaseURL != "https://musicbrainz.org" {
		t.Errorf("BaseURL = %q, want the default", cfg.MusicBrainz.BaseURL)
	}
	if cfg.MusicBrainz.Timeout.Duration != 10*time.Second {
		t.Errorf("Timeout = %v, want the 10s default", cfg.MusicBrainz.Timeout.Duration)
	}
	if cfg.MusicBrainz.CacheTTL.Duration != time.Hour {
		t.Errorf("CacheTTL = %v, want the 1h default", cfg.MusicBrainz.CacheTTL.Duration)
	}
}

func TestLoadMusicBrainzWithoutContactFails(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := string(base) + "\n[musicbrainz]\nbase_url = \"https://musicbrainz.org\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected error for a [musicbrainz] section without a contact")
	}
	if !strings.Contains(err.Error(), "musicbrainz.contact") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

func TestLoadMusicBrainzUnknownKeyFails(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := string(base) + "\n[musicbrainz]\ncontact = \"me@example.com\"\nbogus = \"x\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected error for an unknown musicbrainz key")
	}
	if !strings.Contains(err.Error(), "unknown config keys") {
		t.Errorf("error should report unknown keys: %v", err)
	}
}
