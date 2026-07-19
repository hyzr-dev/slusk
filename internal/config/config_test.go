package config

import (
	"strings"
	"testing"
	"time"
)

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
		{name: "IPv4 loopback without token", listenAddr: "127.0.0.1:9090"},
		{name: "IPv6 loopback without token", listenAddr: "[::1]:9090"},
		{name: "localhost without token", listenAddr: "localhost:9090"},
		{name: "wildcard with token", listenAddr: "0.0.0.0:9090", token: "a-secret-token"},
		{name: "wildcard without token", listenAddr: "0.0.0.0:9090", wantError: "observ.auth_token"},
		{name: "repository placeholder token", listenAddr: "0.0.0.0:9090", token: "REPLACE_WITH_A_LONG_RANDOM_TOKEN", wantError: "must be replaced with a generated token"},
		{name: "token with whitespace", listenAddr: "0.0.0.0:9090", token: "not a bearer token", wantError: "must not contain whitespace"},
		{name: "empty host without token", listenAddr: ":9090", wantError: "observ.auth_token"},
		{name: "LAN address without token", listenAddr: "192.168.1.20:9090", wantError: "observ.auth_token"},
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
