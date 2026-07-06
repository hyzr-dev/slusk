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
	if cfg.Engine.MaxCandidatesPerAlbum != 5 {
		t.Errorf("MaxCandidatesPerAlbum = %d", cfg.Engine.MaxCandidatesPerAlbum)
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
		t.Fatal("expected error for missing status_poll_interval, got nil")
	}
	if !strings.Contains(err.Error(), "status_poll_interval") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

func TestLoadValidHasPipelineFields(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Engine.MinBitrate != 192 {
		t.Errorf("MinBitrate = %d", cfg.Engine.MinBitrate)
	}
	if cfg.Engine.SearchTimeout.Duration.Seconds() != 30 {
		t.Errorf("SearchTimeout = %v", cfg.Engine.SearchTimeout.Duration)
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
