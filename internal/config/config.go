// Package config loads and strictly validates the TOML configuration. Unknown
// keys or sections cause a startup error instead of silently defaulting, which
// is the whole point: a misspelled key must be loud, not silent.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the entire application configuration. Every field maps to a TOML key.
type Config struct {
	Lidarr      LidarrConfig      `toml:"lidarr"`
	Slskd       SlskdConfig       `toml:"slskd"`
	Pipeline    PipelineConfig    `toml:"pipeline"`
	Store       StoreConfig       `toml:"store"`
	Observ      ObservConfig      `toml:"observ"`
	Paths       PathsConfig       `toml:"paths"`
	Soulseek    SoulseekConfig    `toml:"soulseek"`
	MusicBrainz MusicBrainzConfig `toml:"musicbrainz"`
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
	// MaxInflightPerPeer bounds how many files from one candidate are handed to
	// its peer at once. Two jobs using the same peer each get their own budget.
	MaxInflightPerPeer int `toml:"max_inflight_per_peer"`
	// MaxTransferRetries caps how many times a transfer rejected for a
	// transient reason is re-queued before it is given up on.
	MaxTransferRetries int `toml:"max_transfer_retries"`
	// Weights are the tunable scoring weights for the matcher.
	Weights Weights `toml:"weights"`
	// MaxActive caps jobs in DOWNLOADING and IMPORTING. SELECTING uses no slot.
	// Default 30.
	MaxActive int `toml:"max_active"`
	// MaxRetries caps failed search cycles: an empty search or exhausted cached
	// set counts, while candidate failures are free. Default 10.
	MaxRetries int `toml:"max_retries"`
	// BackoffBase sets exponential search-retry backoff; the first delay is
	// twice this value. Default 15m.
	BackoffBase Duration `toml:"backoff_base"`
	// BackoffCap bounds exponential backoff growth. Default 24h.
	BackoffCap Duration `toml:"backoff_cap"`
	// CandidateTTL expires the whole cached candidate set and resets the job to
	// WANTED for a fresh search. Default 24h.
	CandidateTTL Duration `toml:"candidate_ttl"`
	// FailedReviveAfter is how long a permanently failed job waits before
	// being revived for another attempt. Default 720h (30 days).
	FailedReviveAfter Duration `toml:"failed_revive_after"`
	// StuckAfter is the maximum time an IMPORTING job may remain without progress
	// in pre-submit verification due to repeated verification failures or anomalies;
	// after that, the current candidate is failed and the next is tried. Default 1h.
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
	// ManualImportTimeout bounds Lidarr's manualimport folder scan, which
	// parses audio tags per file and legitimately runs far longer than a
	// normal API call on large folders (box sets, deluxe editions).
	// Default 10m.
	ManualImportTimeout Duration `toml:"manual_import_timeout"`
	// ImportRetryCooldown is how long the importing phase waits before
	// re-attempting a failed manualimport folder scan on the same job, so a
	// slow-scanning folder is not hammered every ImportingInterval until
	// StuckAfter elapses. Default 5m.
	ImportRetryCooldown Duration `toml:"import_retry_cooldown"`
	// Backend selects which peer backend drives the pipeline: BackendSlskd
	// (the slskd daemon, the default) or BackendSoulseek (the native
	// internal/soulseek client). Default BackendSlskd.
	Backend string `toml:"backend"`
}

// Backend selects the peer backend that drives the pipeline.
const (
	// BackendSlskd routes peer operations through the slskd daemon.
	BackendSlskd = "slskd"
	// BackendSoulseek routes peer operations through the native
	// internal/soulseek client, connecting directly to the Soulseek server.
	BackendSoulseek = "soulseek"
)

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
	if p.ManualImportTimeout.Duration == 0 {
		p.ManualImportTimeout.Duration = 10 * time.Minute
	}
	if p.ImportRetryCooldown.Duration == 0 {
		p.ImportRetryCooldown.Duration = 5 * time.Minute
	}
	if p.Backend == "" {
		p.Backend = BackendSlskd
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
	// AuthToken is an OPTIONAL bearer/Basic credential for machine/API access
	// (curl, Prometheus, the Vite dev proxy) alongside the browser's
	// form-based session login (issue #279), which is mandatory and needs no
	// config key. Blank disables machine access; it no longer makes a
	// non-loopback listener unprotected.
	AuthToken string `toml:"auth_token"`
	// LogLevel is the minimum slog level emitted: "debug", "info" (default),
	// "warn", or "error". Validate rejects any other non-empty value.
	LogLevel string `toml:"log_level"`
}

// logLevels maps accepted observ.log_level values (case-insensitive) to slog
// levels; an empty value defaults to info.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// SlogLevel returns the slog.Level selected by LogLevel, defaulting to
// slog.LevelInfo when unset. Validate rejects any other value, so a typo is
// caught at startup rather than silently swallowed here.
func (o ObservConfig) SlogLevel() slog.Level {
	if o.LogLevel == "" {
		return slog.LevelInfo
	}
	return logLevels[strings.ToLower(o.LogLevel)]
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
type SharedFolderConfig struct {
	// Name is the public, virtual root exposed to Soulseek peers. It never
	// reveals Path.
	Name string `toml:"name"`
	// Path is an absolute local directory. Share mounts should be read-only.
	Path string `toml:"path"`
}

// GluetunConfig lets the native Soulseek client fetch its listen port from a
// gluetun VPN container's control server at startup instead of using the
// static port in ListenAddr. Absent (blank ControlURL) means disabled.
type GluetunConfig struct {
	ControlURL string `toml:"control_url"`
	APIKey     string `toml:"api_key"`
}

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
	// Gluetun, when set, makes the native client fetch its forwarded port
	// from a gluetun VPN container's control server at startup and use it in
	// place of ListenAddr's port; ListenAddr's host is still used.
	Gluetun GluetunConfig `toml:"gluetun"`
	// SharedFolders are explicitly named local roots. Absolute local paths are
	// never sent to peers; only Name and paths below it are public.
	SharedFolders []SharedFolderConfig `toml:"shared_folders"`
	// UploadSlots limits concurrent upload negotiation and streaming. Default 2.
	UploadSlots int `toml:"upload_slots"`
	// AllowPrivatePeerAddresses permits dialing server-supplied peer
	// addresses in RFC 1918 / ULA private ranges (threat T12). Loopback and
	// link-local addresses are always refused regardless of this flag.
	// Defaults to false (private addresses blocked); set true to reach
	// peers on your own LAN.
	AllowPrivatePeerAddresses bool `toml:"allow_private_peer_addresses"`
}

// Enabled reports whether any field of the section was set, meaning the
// direct Soulseek connection should be started.
func (s SoulseekConfig) Enabled() bool {
	return s.ServerAddress != "" || s.Username != "" || s.Password != "" || s.ListenAddr != "" || len(s.SharedFolders) != 0 || s.UploadSlots != 0 || s.Gluetun.ControlURL != "" || s.Gluetun.APIKey != ""
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
	if s.UploadSlots == 0 {
		s.UploadSlots = 2
	}
}

const defaultMusicBrainzBaseURL = "https://musicbrainz.org"
const defaultMusicBrainzTimeout = 10 * time.Second
const defaultMusicBrainzCacheTTL = time.Hour

// MusicBrainzConfig enables identifying a Soulseek search result against
// MusicBrainz (issue #321). The whole section is optional: a wholly absent
// [musicbrainz] section, or one with every field blank, leaves it disabled -
// the identify endpoints then answer "not enabled" rather than crashing the
// process, which matters here more than for most sections since this key did
// not exist before this PR and merging to main auto-deploys (see CLAUDE.md).
type MusicBrainzConfig struct {
	// BaseURL is the MusicBrainz web service root. Defaults to
	// https://musicbrainz.org when the section is enabled and this is blank.
	BaseURL string `toml:"base_url"`
	// Contact identifies this application in the User-Agent MusicBrainz's
	// usage policy requires (an email address or a URL) - required with no
	// default, since sending an unidentified request risks the whole app's
	// IP being blocked (see internal/musicbrainz.ErrNoContact).
	Contact string `toml:"contact"`
	// Timeout bounds a single HTTP request. Defaults to 10s.
	Timeout Duration `toml:"timeout"`
	// CacheTTL is how long a response is reused before it is fetched again.
	// Defaults to 1h.
	CacheTTL Duration `toml:"cache_ttl"`
}

// Enabled reports whether any field of the section was set, meaning the
// identify feature should be wired up.
func (m MusicBrainzConfig) Enabled() bool {
	return m.BaseURL != "" || m.Contact != "" || m.Timeout.Duration != 0 || m.CacheTTL.Duration != 0
}

// applyDefaults fills BaseURL, Timeout and CacheTTL with their documented
// defaults when the section is enabled and the field was left blank. Contact
// has no default - see its doc comment - so Validate rejects it being blank.
func (m *MusicBrainzConfig) applyDefaults() {
	if !m.Enabled() {
		return
	}
	if m.BaseURL == "" {
		m.BaseURL = defaultMusicBrainzBaseURL
	}
	if m.Timeout.Duration == 0 {
		m.Timeout.Duration = defaultMusicBrainzTimeout
	}
	if m.CacheTTL.Duration == 0 {
		m.CacheTTL.Duration = defaultMusicBrainzCacheTTL
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return LoadBytes(data)
}

// LoadBytes strictly decodes TOML data already in memory, performing the
// identical unknown-key check, defaulting, and validation as Load. It exists
// so internal/config/write.go can verify a rendered document is safe to write
// to disk before it ever touches disk (see ApplySettings).
func LoadBytes(data []byte) (Config, error) {
	var cfg Config
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("unknown config keys: %v", undecoded)
	}
	// Zero means absent for the normal defaulting path, but an explicitly
	// configured nonpositive slot count is a typo and must fail loudly.
	if meta.IsDefined("soulseek", "upload_slots") && cfg.Soulseek.UploadSlots <= 0 {
		return Config{}, errors.New("soulseek.upload_slots must be > 0")
	}
	cfg.Pipeline.applyDefaults()
	cfg.Soulseek.applyDefaults()
	cfg.MusicBrainz.applyDefaults()
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
	switch c.Pipeline.Backend {
	case BackendSlskd:
		if c.Slskd.URL == "" {
			problems = append(problems, "slskd.url is required")
		}
		if c.Slskd.APIKey == "" {
			problems = append(problems, "slskd.api_key is required")
		}
	case BackendSoulseek:
		if !c.Soulseek.Enabled() {
			problems = append(problems, "pipeline.backend = \"soulseek\" requires a configured [soulseek] section")
		}
	default:
		problems = append(problems, fmt.Sprintf("pipeline.backend must be %q or %q", BackendSlskd, BackendSoulseek))
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
	if c.Pipeline.ManualImportTimeout.Duration <= 0 {
		problems = append(problems, "pipeline.manual_import_timeout must be > 0")
	}
	if c.Pipeline.ImportRetryCooldown.Duration <= 0 {
		problems = append(problems, "pipeline.import_retry_cooldown must be > 0")
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
			problems = append(problems, "soulseek.listen_addr port must be between 1 and 65535")
		}
		if g := c.Soulseek.Gluetun; g.ControlURL != "" {
			u, err := url.Parse(g.ControlURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				problems = append(problems, "soulseek.gluetun.control_url must be an http(s) URL")
			}
		} else if g.APIKey != "" {
			problems = append(problems, "soulseek.gluetun.api_key requires soulseek.gluetun.control_url")
		}
		if c.Soulseek.UploadSlots <= 0 {
			problems = append(problems, "soulseek.upload_slots must be > 0")
		}
		names := make(map[string]struct{}, len(c.Soulseek.SharedFolders))
		paths := make(map[string]struct{}, len(c.Soulseek.SharedFolders))
		for i, share := range c.Soulseek.SharedFolders {
			name := strings.TrimSpace(share.Name)
			if name != share.Name {
				problems = append(problems, fmt.Sprintf("soulseek.shared_folders[%d].name must not contain surrounding whitespace", i))
			} else if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
				problems = append(problems, fmt.Sprintf("soulseek.shared_folders[%d].name must be nonblank and contain no path separators", i))
			} else if _, exists := names[strings.ToLower(name)]; exists {
				problems = append(problems, fmt.Sprintf("soulseek.shared_folders[%d].name must be unique", i))
			} else {
				names[strings.ToLower(name)] = struct{}{}
			}
			path := strings.TrimSpace(share.Path)
			if path != share.Path {
				problems = append(problems, fmt.Sprintf("soulseek.shared_folders[%d].path must not contain surrounding whitespace", i))
			} else if path == "" || !filepath.IsAbs(path) {
				problems = append(problems, fmt.Sprintf("soulseek.shared_folders[%d].path must be an absolute path", i))
			} else {
				clean := filepath.Clean(path)
				if _, exists := paths[clean]; exists {
					problems = append(problems, fmt.Sprintf("soulseek.shared_folders[%d].path must be unique", i))
				} else {
					paths[clean] = struct{}{}
				}
			}
		}
	}
	if c.MusicBrainz.Enabled() {
		if c.MusicBrainz.Contact == "" {
			problems = append(problems, "musicbrainz.contact is required when the musicbrainz section is enabled")
		}
		if c.MusicBrainz.Timeout.Duration <= 0 {
			problems = append(problems, "musicbrainz.timeout must be > 0")
		}
		if c.MusicBrainz.CacheTTL.Duration <= 0 {
			problems = append(problems, "musicbrainz.cache_ttl must be > 0")
		}
	}
	if c.Observ.ListenAddr == "" {
		problems = append(problems, "observ.listen_addr is required")
	} else {
		// A missing token no longer makes a non-loopback listener
		// unprotected (issue #279): form-based session login is now
		// mandatory browser-side auth regardless of this setting, so an
		// empty token just means "no machine/API credential is accepted" -
		// see internal/observ.TokenAuthenticator.
		_, _, err := net.SplitHostPort(c.Observ.ListenAddr)
		if err != nil {
			problems = append(problems, "observ.listen_addr must be a valid host:port")
		}
		if c.Observ.AuthToken == observAuthTokenPlaceholder {
			problems = append(problems, "observ.auth_token must be replaced with a generated token")
		}
		if strings.ContainsAny(c.Observ.AuthToken, " \t\r\n") {
			problems = append(problems, "observ.auth_token must not contain whitespace")
		}
	}
	if c.Observ.LogLevel != "" {
		if _, ok := logLevels[strings.ToLower(c.Observ.LogLevel)]; !ok {
			problems = append(problems, fmt.Sprintf("observ.log_level must be one of debug, info, warn, error (got %q)", c.Observ.LogLevel))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}
