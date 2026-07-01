package config

import (
	"strings"
	"testing"
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
