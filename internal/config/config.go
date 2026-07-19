// Package config loads and strictly validates the TOML configuration. Unknown
// keys or sections cause a startup error instead of silently defaulting, which
// is the whole point: a misspelled key must be loud, not silent.
package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the entire application configuration. Every field maps to a TOML key.
type Config struct {
	Lidarr   LidarrConfig   `toml:"lidarr"`
	Slskd    SlskdConfig    `toml:"slskd"`
	Pipeline PipelineConfig `toml:"pipeline"`
	Store    StoreConfig    `toml:"store"`
	Observ   ObservConfig   `toml:"observ"`
	Paths    PathsConfig    `toml:"paths"`
	Soulseek SoulseekConfig `toml:"soulseek"`
}

// LidarrConfig is the Lidarr music server integration configuration. There is
// no poll_interval here: the legacy engine's LidarrPoll tick is gone, and the
// pipeline's own wanted-list refresh cadence is [pipeline] wanted_sync_interval.
type LidarrConfig struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

// SlskdConfig is the Slsk daemon configuration. There is no
// status_poll_interval here: the legacy engine's StatusPoll tick is gone, and
// the pipeline's own downloading-reconcile cadence is [pipeline]
// downloading_interval.
type SlskdConfig struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

// Weights are the tunable scoring weights for the matcher.
type Weights struct {
	Format      float64 `toml:"format"`
	Bitrate     float64 `toml:"bitrate"`
	Reliability float64 `toml:"reliability"`
	FileCount   float64 `toml:"file_count"`
	// KnownUser weights the peer's decayed artist/global success-fail history
	// (0..1, see matcher.ReliabilityHistoryScore) into the candidate score, so a
	// peer who has previously delivered a complete, importable release for this
	// artist (or is a known-good/known-bad peer in general) is boosted or
	// suppressed relative to an unknown peer.
	KnownUser float64 `toml:"known_user"`
}

// PipelineConfig is the state-machine pipeline configuration: the sole
// configuration surface for matching, transfer and scheduling knobs (the
// legacy [engine] section it replaced is gone). The scheduling/backoff knobs
// below are optional (a wholly absent [pipeline] section yields the defaults
// applied in applyDefaults); the matcher/transfer knobs (MaxCandidatesPerAlbum
// through Weights) are mandatory, same as they were under [engine].
type PipelineConfig struct {
	// MaxCandidatesPerAlbum bounds how many ranked search results are cached
	// per album search.
	MaxCandidatesPerAlbum int `toml:"max_candidates_per_album"`
	// TransferDeadline bounds how long a single file transfer may run before
	// it is considered overdue.
	TransferDeadline Duration `toml:"transfer_deadline"`
	// StallTimeout bounds how long a transfer may go without byte progress
	// before it is considered stalled.
	StallTimeout Duration `toml:"stall_timeout"`
	// SearchTimeout bounds how long a single Soulseek search waits for results.
	SearchTimeout Duration `toml:"search_timeout"`
	// MinBitrate rejects candidate files below this bitrate (kbps).
	MinBitrate int `toml:"min_bitrate"`
	// MaxInflightPerPeer bounds how many files are handed to a single peer at
	// once, so a burst never trips a peer's per-user queued-megabyte limit.
	MaxInflightPerPeer int `toml:"max_inflight_per_peer"`
	// MaxTransferRetries caps how many times a transfer rejected for a
	// transient reason is re-queued before it is given up on.
	MaxTransferRetries int `toml:"max_transfer_retries"`
	// Weights are the tunable scoring weights for the matcher.
	Weights Weights `toml:"weights"`
	// MaxActive caps jobs simultaneously active across the pipeline
	// (SELECTING/DOWNLOADING/IMPORTING). Default 30.
	MaxActive int `toml:"max_active"`
	// MaxRetries caps how many candidates a job cycles through before it is
	// given up on. Default 10.
	MaxRetries int `toml:"max_retries"`
	// BackoffBase is the initial wait before retrying a job with no untried
	// candidate left. Default 15m.
	BackoffBase Duration `toml:"backoff_base"`
	// BackoffCap bounds exponential backoff growth. Default 24h.
	BackoffCap Duration `toml:"backoff_cap"`
	// CandidateTTL is how long a discovered candidate stays eligible before
	// it is dropped and re-discovered. Default 24h.
	CandidateTTL Duration `toml:"candidate_ttl"`
	// FailedReviveAfter is how long a permanently failed job waits before
	// being revived for another attempt. Default 720h (30 days).
	FailedReviveAfter Duration `toml:"failed_revive_after"`
	// StuckAfter is how long a job may sit without progress in an active
	// phase before it is treated as stuck and reconciled. Default 1h.
	StuckAfter Duration `toml:"stuck_after"`
	// TickTimeout bounds a single pipeline tick's total execution time.
	// Default 5m.
	TickTimeout Duration `toml:"tick_timeout"`
	// ImportConfirmTimeout is how long an import may sit unconfirmed before
	// it's treated as failed and rotated to the next candidate. Default 3m.
	ImportConfirmTimeout Duration `toml:"import_confirm_timeout"`
	// WantedSyncInterval is how often the wanted list is refreshed from
	// Lidarr. Default 15m.
	WantedSyncInterval Duration `toml:"wanted_sync_interval"`
	// DiscoveryInterval is how often the discovery phase runs. Default 30s.
	DiscoveryInterval Duration `toml:"discovery_interval"`
	// SelectingInterval is how often the selecting phase runs. Default 10s.
	SelectingInterval Duration `toml:"selecting_interval"`
	// DownloadingInterval is how often the downloading phase runs. Default 15s.
	DownloadingInterval Duration `toml:"downloading_interval"`
	// ImportingInterval is how often the importing phase runs. Default 30s.
	ImportingInterval Duration `toml:"importing_interval"`
}

// applyDefaults fills any zero-valued field with its documented default.
// Note: an explicit zero in TOML (e.g. `max_active = 0`) is indistinguishable
// from an absent key and silently takes the default rather than failing Validate.
// Accepted: no pipeline field is legitimately zero.
func (p *PipelineConfig) applyDefaults() {
	if p.MaxActive == 0 {
		p.MaxActive = 30
	}
	if p.MaxRetries == 0 {
		p.MaxRetries = 10
	}
	if p.BackoffBase.Duration == 0 {
		p.BackoffBase.Duration = 15 * time.Minute
	}
	if p.BackoffCap.Duration == 0 {
		p.BackoffCap.Duration = 24 * time.Hour
	}
	if p.CandidateTTL.Duration == 0 {
		p.CandidateTTL.Duration = 24 * time.Hour
	}
	if p.FailedReviveAfter.Duration == 0 {
		p.FailedReviveAfter.Duration = 720 * time.Hour
	}
	if p.StuckAfter.Duration == 0 {
		p.StuckAfter.Duration = time.Hour
	}
	if p.TickTimeout.Duration == 0 {
		p.TickTimeout.Duration = 5 * time.Minute
	}
	if p.ImportConfirmTimeout.Duration == 0 {
		p.ImportConfirmTimeout.Duration = 3 * time.Minute
	}
	if p.WantedSyncInterval.Duration == 0 {
		p.WantedSyncInterval.Duration = 15 * time.Minute
	}
	if p.DiscoveryInterval.Duration == 0 {
		p.DiscoveryInterval.Duration = 30 * time.Second
	}
	if p.SelectingInterval.Duration == 0 {
		p.SelectingInterval.Duration = 10 * time.Second
	}
	if p.DownloadingInterval.Duration == 0 {
		p.DownloadingInterval.Duration = 15 * time.Second
	}
	if p.ImportingInterval.Duration == 0 {
		p.ImportingInterval.Duration = 30 * time.Second
	}
}

// StoreConfig is the persistent store configuration.
type StoreConfig struct {
	// DSN is the PostgreSQL connection string, e.g.
	// postgres://slskdarr:password@postgres:5432/slskdarr?sslmode=disable
	DSN string `toml:"dsn"`
}

const observAuthTokenPlaceholder = "REPLACE_WITH_A_LONG_RANDOM_TOKEN"

// ObservConfig is the observability configuration.
type ObservConfig struct {
	ListenAddr string `toml:"listen_addr"`
	// AuthToken protects every UI, API, and metrics endpoint except /healthz.
	// It may be omitted only when ListenAddr is strictly loopback-only.
	AuthToken string `toml:"auth_token"`
}

// PathsConfig holds filesystem paths shared with the arr-stack.
type PathsConfig struct {
	SlskdCompleteDir string `toml:"slskd_complete_dir"`
}

const defaultSoulseekServerAddress = "server.slsknet.org:2242"
const defaultSoulseekListenAddr = "0.0.0.0:2234"

// SoulseekConfig configures the direct connection to the central Soulseek
// server (as opposed to the [slskd] daemon). The whole section is optional:
// a wholly absent [soulseek] section, or one with every field blank, leaves
// the direct connection disabled.
type SoulseekConfig struct {
	// ServerAddress is the Soulseek server's host:port. Defaults to
	// server.slsknet.org:2242 when the section is enabled and this is blank.
	ServerAddress string `toml:"server_address"`
	Username      string `toml:"username"`
	Password      string `toml:"password"`
	// ListenAddr is the host:port slskdarr listens on for incoming peer
	// connections, advertised to the server after login. Defaults to
	// 0.0.0.0:2234 when the section is enabled and this is blank. Peers must
	// be able to reach this port: with Docker port mapping, the published
	// host port must equal it (there is no separate "advertised port"
	// concept - the listen port itself is what gets advertised).
	ListenAddr string `toml:"listen_addr"`
}

// Enabled reports whether any field of the section was set, meaning the
// direct Soulseek connection should be started.
func (s SoulseekConfig) Enabled() bool {
	return s.ServerAddress != "" || s.Username != "" || s.Password != "" || s.ListenAddr != ""
}

// applyDefaults fills ServerAddress and ListenAddr with their documented
// defaults when the section is enabled and the field was left blank.
func (s *SoulseekConfig) applyDefaults() {
	if !s.Enabled() {
		return
	}
	if s.ServerAddress == "" {
		s.ServerAddress = defaultSoulseekServerAddress
	}
	if s.ListenAddr == "" {
		s.ListenAddr = defaultSoulseekListenAddr
	}
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
	cfg.Pipeline.applyDefaults()
	cfg.Soulseek.applyDefaults()
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
	if c.Slskd.URL == "" {
		problems = append(problems, "slskd.url is required")
	}
	if c.Slskd.APIKey == "" {
		problems = append(problems, "slskd.api_key is required")
	}
	if c.Pipeline.MaxCandidatesPerAlbum <= 0 {
		problems = append(problems, "pipeline.max_candidates_per_album must be > 0")
	}
	if c.Pipeline.TransferDeadline.Duration <= 0 {
		problems = append(problems, "pipeline.transfer_deadline must be > 0")
	}
	if c.Pipeline.StallTimeout.Duration <= 0 {
		problems = append(problems, "pipeline.stall_timeout must be > 0")
	}
	if c.Pipeline.SearchTimeout.Duration <= 0 {
		problems = append(problems, "pipeline.search_timeout must be > 0")
	}
	if c.Pipeline.MinBitrate <= 0 {
		problems = append(problems, "pipeline.min_bitrate must be > 0")
	}
	if c.Pipeline.MaxInflightPerPeer <= 0 {
		problems = append(problems, "pipeline.max_inflight_per_peer must be > 0")
	}
	if c.Pipeline.MaxTransferRetries < 0 {
		problems = append(problems, "pipeline.max_transfer_retries must be >= 0")
	}
	if c.Pipeline.Weights.KnownUser < 0 {
		problems = append(problems, "pipeline.weights.known_user must be >= 0")
	}
	if c.Pipeline.MaxActive < 1 {
		problems = append(problems, "pipeline.max_active must be >= 1")
	}
	if c.Pipeline.MaxRetries < 1 {
		problems = append(problems, "pipeline.max_retries must be >= 1")
	}
	if c.Pipeline.BackoffBase.Duration <= 0 {
		problems = append(problems, "pipeline.backoff_base must be > 0")
	}
	if c.Pipeline.BackoffCap.Duration <= 0 {
		problems = append(problems, "pipeline.backoff_cap must be > 0")
	}
	if c.Pipeline.CandidateTTL.Duration <= 0 {
		problems = append(problems, "pipeline.candidate_ttl must be > 0")
	}
	if c.Pipeline.FailedReviveAfter.Duration <= 0 {
		problems = append(problems, "pipeline.failed_revive_after must be > 0")
	}
	if c.Pipeline.StuckAfter.Duration <= 0 {
		problems = append(problems, "pipeline.stuck_after must be > 0")
	}
	if c.Pipeline.TickTimeout.Duration <= 0 {
		problems = append(problems, "pipeline.tick_timeout must be > 0")
	}
	if c.Pipeline.ImportConfirmTimeout.Duration <= 0 {
		problems = append(problems, "pipeline.import_confirm_timeout must be > 0")
	}
	if c.Pipeline.WantedSyncInterval.Duration <= 0 {
		problems = append(problems, "pipeline.wanted_sync_interval must be > 0")
	}
	if c.Pipeline.DiscoveryInterval.Duration <= 0 {
		problems = append(problems, "pipeline.discovery_interval must be > 0")
	}
	if c.Pipeline.SelectingInterval.Duration <= 0 {
		problems = append(problems, "pipeline.selecting_interval must be > 0")
	}
	if c.Pipeline.DownloadingInterval.Duration <= 0 {
		problems = append(problems, "pipeline.downloading_interval must be > 0")
	}
	if c.Pipeline.ImportingInterval.Duration <= 0 {
		problems = append(problems, "pipeline.importing_interval must be > 0")
	}
	if c.Paths.SlskdCompleteDir == "" {
		problems = append(problems, "paths.slskd_complete_dir is required")
	}
	if c.Store.DSN == "" {
		problems = append(problems, "store.dsn is required")
	}
	if c.Soulseek.Enabled() {
		if c.Soulseek.Username == "" {
			problems = append(problems, "soulseek.username is required when the soulseek section is enabled")
		}
		if c.Soulseek.Password == "" {
			problems = append(problems, "soulseek.password is required when the soulseek section is enabled")
		}
		if _, _, err := net.SplitHostPort(c.Soulseek.ServerAddress); err != nil {
			problems = append(problems, "soulseek.server_address must be a valid host:port")
		}
		if _, port, err := net.SplitHostPort(c.Soulseek.ListenAddr); err != nil {
			problems = append(problems, "soulseek.listen_addr must be a valid host:port")
		} else if portNum, err := strconv.Atoi(port); err != nil {
			// net.SplitHostPort only checks the address shape, not that the
			// port is numeric: "0.0.0.0:abc" passes it and would otherwise
			// only fail much later, at bind time.
			problems = append(problems, "soulseek.listen_addr must have a numeric port")
		} else if portNum <= 0 || portNum > 65535 {
			problems = append(problems, "soulseek.listen_addr must have a nonzero port")
		}
	}
	if c.Observ.ListenAddr == "" {
		problems = append(problems, "observ.listen_addr is required")
	} else {
		host, _, err := net.SplitHostPort(c.Observ.ListenAddr)
		if err != nil {
			problems = append(problems, "observ.listen_addr must be a valid host:port")
		} else if c.Observ.AuthToken == "" && !isLoopbackHost(host) {
			problems = append(problems, "observ.auth_token is required when observ.listen_addr is not loopback-only")
		}
		if c.Observ.AuthToken == observAuthTokenPlaceholder {
			problems = append(problems, "observ.auth_token must be replaced with a generated token")
		}
		if strings.ContainsAny(c.Observ.AuthToken, " \t\r\n") {
			problems = append(problems, "observ.auth_token must not contain whitespace")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
