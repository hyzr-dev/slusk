// Package config loads and strictly validates the TOML configuration. Unknown
// keys or sections cause a startup error instead of silently defaulting, which
// is the whole point: a misspelled key must be loud, not silent.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the entire application configuration. Every field maps to a TOML key.
type Config struct {
	Lidarr LidarrConfig `toml:"lidarr"`
	Slskd  SlskdConfig  `toml:"slskd"`
	Engine EngineConfig `toml:"engine"`
	Store  StoreConfig  `toml:"store"`
	Observ ObservConfig `toml:"observ"`
}

// LidarrConfig is the Lidarr music server integration configuration.
type LidarrConfig struct {
	URL          string   `toml:"url"`
	APIKey       string   `toml:"api_key"`
	PollInterval Duration `toml:"poll_interval"`
}

// SlskdConfig is the Slsk daemon configuration.
type SlskdConfig struct {
	URL                string   `toml:"url"`
	APIKey             string   `toml:"api_key"`
	StatusPollInterval Duration `toml:"status_poll_interval"`
}

// EngineConfig is the matching engine configuration.
type EngineConfig struct {
	MaxCandidatesPerAlbum int      `toml:"max_candidates_per_album"`
	TransferDeadline      Duration `toml:"transfer_deadline"`
	StallTimeout          Duration `toml:"stall_timeout"`
	Weights               Weights  `toml:"weights"`
}

// Weights are the tunable scoring weights for the matcher.
type Weights struct {
	Format      float64 `toml:"format"`
	Bitrate     float64 `toml:"bitrate"`
	Reliability float64 `toml:"reliability"`
	FileCount   float64 `toml:"file_count"`
}

// StoreConfig is the persistent store configuration.
type StoreConfig struct {
	Path string `toml:"path"`
}

// ObservConfig is the observability configuration.
type ObservConfig struct {
	ListenAddr string `toml:"listen_addr"`
}

// Duration wraps time.Duration so TOML strings like "5m" decode directly.
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string (e.g. "5m", "15s").
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

// Load reads and strictly decodes the config at path. Any key present in the
// file but absent from Config is reported as an error.
func Load(path string) (Config, error) {
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("unknown config keys: %v", undecoded)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports the first set of missing or non-positive required fields.
// It runs as part of Load so a missing key fails loudly at startup, just like
// an unknown key does.
func (c Config) Validate() error {
	var problems []string
	if c.Lidarr.URL == "" {
		problems = append(problems, "lidarr.url is required")
	}
	if c.Lidarr.APIKey == "" {
		problems = append(problems, "lidarr.api_key is required")
	}
	if c.Lidarr.PollInterval.Duration <= 0 {
		problems = append(problems, "lidarr.poll_interval must be > 0")
	}
	if c.Slskd.URL == "" {
		problems = append(problems, "slskd.url is required")
	}
	if c.Slskd.APIKey == "" {
		problems = append(problems, "slskd.api_key is required")
	}
	if c.Slskd.StatusPollInterval.Duration <= 0 {
		problems = append(problems, "slskd.status_poll_interval must be > 0")
	}
	if c.Engine.MaxCandidatesPerAlbum <= 0 {
		problems = append(problems, "engine.max_candidates_per_album must be > 0")
	}
	if c.Engine.TransferDeadline.Duration <= 0 {
		problems = append(problems, "engine.transfer_deadline must be > 0")
	}
	if c.Engine.StallTimeout.Duration <= 0 {
		problems = append(problems, "engine.stall_timeout must be > 0")
	}
	if c.Store.Path == "" {
		problems = append(problems, "store.path is required")
	}
	if c.Observ.ListenAddr == "" {
		problems = append(problems, "observ.listen_addr is required")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}
