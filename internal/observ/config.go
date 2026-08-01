// Package observ: config.go serves the settings view's view of the running
// configuration, its connection tests, and (see issues #89/#134) the writable
// settings update endpoint. Secrets are never sent — only whether they are
// set, and connection tests probe the loaded config server-side rather than
// any client-supplied values. Updates take effect by restarting the process
// so the whole application always runs off one freshly-loaded config.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// --- GET /api/config response shape ---
//
// Every nested view struct below is fully exported with no unexported
// fields: unlike v1 (which kept the raw Lidarr API key in an unexported
// field and computed "configured" at serve time), main.go now computes each
// ...Configured boolean itself (it already holds the loaded config.Config
// with the real secrets) and passes only booleans in. No secret value is
// ever held by, or reachable from, an observ.AppConfig.

// LidarrView is the settings view's rendering of LidarrConfig.
type LidarrView struct {
	URL              string `json:"url"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
}

// SlskdView is the settings view's rendering of SlskdConfig.
type SlskdView struct {
	URL              string `json:"url"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
}

// WeightsView is the settings view's rendering of config.Weights.
type WeightsView struct {
	Format      float64 `json:"format"`
	Bitrate     float64 `json:"bitrate"`
	Reliability float64 `json:"reliability"`
	FileCount   float64 `json:"fileCount"`
	KnownUser   float64 `json:"knownUser"`
}

// PipelineView is the settings view's rendering of PipelineConfig. Durations
// are Go's canonical string form (e.g. "1h0m0s"), matching
// time.Duration.String, so main.go can pass them through unchanged.
type PipelineView struct {
	Backend               string      `json:"backend"`
	MaxCandidatesPerAlbum int         `json:"maxCandidatesPerAlbum"`
	MaxActive             int         `json:"maxActive"`
	MaxRetries            int         `json:"maxRetries"`
	MaxInflightPerPeer    int         `json:"maxInflightPerPeer"`
	MaxTransferRetries    int         `json:"maxTransferRetries"`
	MinBitrate            int         `json:"minBitrate"`
	TransferDeadline      string      `json:"transferDeadline"`
	StallTimeout          string      `json:"stallTimeout"`
	SearchTimeout         string      `json:"searchTimeout"`
	BackoffBase           string      `json:"backoffBase"`
	BackoffCap            string      `json:"backoffCap"`
	CandidateTTL          string      `json:"candidateTtl"`
	FailedReviveAfter     string      `json:"failedReviveAfter"`
	StuckAfter            string      `json:"stuckAfter"`
	TickTimeout           string      `json:"tickTimeout"`
	ImportConfirmTimeout  string      `json:"importConfirmTimeout"`
	WantedSyncInterval    string      `json:"wantedSyncInterval"`
	DiscoveryInterval     string      `json:"discoveryInterval"`
	SelectingInterval     string      `json:"selectingInterval"`
	DownloadingInterval   string      `json:"downloadingInterval"`
	ImportingInterval     string      `json:"importingInterval"`
	ManualImportTimeout   string      `json:"manualImportTimeout"`
	ImportRetryCooldown   string      `json:"importRetryCooldown"`
	Weights               WeightsView `json:"weights"`
}

// GluetunView is the settings view's rendering of GluetunConfig.
type GluetunView struct {
	ControlURL       string `json:"controlUrl"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
}

// SharedFolderView is the settings view's rendering of one SharedFolderConfig
// entry. Path is a local filesystem path, but never leaves the server to
// anyone but the operator loading their own settings view, same as today.
type SharedFolderView struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SoulseekView is the settings view's rendering of SoulseekConfig. Enabled
// mirrors SoulseekConfig.Enabled() — it is derived from the other fields, not
// an independent setting, and is therefore GET-only (POST does not carry it).
type SoulseekView struct {
	Enabled                   bool               `json:"enabled"`
	ServerAddress             string             `json:"serverAddress"`
	Username                  string             `json:"username"`
	PasswordConfigured        bool               `json:"passwordConfigured"`
	ListenAddr                string             `json:"listenAddr"`
	UploadSlots               int                `json:"uploadSlots"`
	AllowPrivatePeerAddresses bool               `json:"allowPrivatePeerAddresses"`
	Gluetun                   GluetunView        `json:"gluetun"`
	SharedFolders             []SharedFolderView `json:"sharedFolders"`
}

// StoreView is the settings view's rendering of StoreConfig.
type StoreView struct {
	DSNConfigured bool `json:"dsnConfigured"`
}

// ObservView is the settings view's rendering of ObservConfig.
type ObservView struct {
	ListenAddr          string `json:"listenAddr"`
	AuthTokenConfigured bool   `json:"authTokenConfigured"`
	LogLevel            string `json:"logLevel"`
}

// PathsView is the settings view's rendering of PathsConfig.
type PathsView struct {
	SlskdCompleteDir string `json:"slskdCompleteDir"`
}

// AppConfig is the full configuration the settings view displays, mirroring
// config.Config's section tree. Writable reports whether the config file's
// directory currently accepts writes (see config.ProbeWritable), so the
// settings view can render an editable form or a read-only one without first
// attempting a write.
type AppConfig struct {
	Lidarr   LidarrView   `json:"lidarr"`
	Slskd    SlskdView    `json:"slskd"`
	Pipeline PipelineView `json:"pipeline"`
	Soulseek SoulseekView `json:"soulseek"`
	Store    StoreView    `json:"store"`
	Observ   ObservView   `json:"observ"`
	Paths    PathsView    `json:"paths"`
	Writable bool         `json:"writable"`
}

// ConfigFunc produces the current display configuration.
type ConfigFunc func() AppConfig

// --- POST /api/config request/update shape ---
//
// Each *Update struct below mirrors the identically-named config.*Settings
// struct in internal/config one-for-one; main.go performs the trivial
// field-by-field conversion (observ deliberately does not import
// internal/config — see the package comment). A nil secret pointer means
// "keep the currently configured value": the settings view never receives a
// configured secret back, so it has no way to resend it unchanged.

type LidarrUpdate struct {
	URL    string
	APIKey *string
}

type SlskdUpdate struct {
	URL    string
	APIKey *string
}

type WeightsUpdate struct {
	Format      float64
	Bitrate     float64
	Reliability float64
	FileCount   float64
	KnownUser   float64
}

type PipelineUpdate struct {
	Backend               string
	MaxCandidatesPerAlbum int
	MaxActive             int
	MaxRetries            int
	MaxInflightPerPeer    int
	MaxTransferRetries    int
	MinBitrate            int
	TransferDeadline      time.Duration
	StallTimeout          time.Duration
	SearchTimeout         time.Duration
	BackoffBase           time.Duration
	BackoffCap            time.Duration
	CandidateTTL          time.Duration
	FailedReviveAfter     time.Duration
	StuckAfter            time.Duration
	TickTimeout           time.Duration
	ImportConfirmTimeout  time.Duration
	WantedSyncInterval    time.Duration
	DiscoveryInterval     time.Duration
	SelectingInterval     time.Duration
	DownloadingInterval   time.Duration
	ImportingInterval     time.Duration
	ManualImportTimeout   time.Duration
	ImportRetryCooldown   time.Duration
	Weights               WeightsUpdate
}

type GluetunUpdate struct {
	ControlURL string
	APIKey     *string
}

type SharedFolderUpdate struct {
	Name string
	Path string
}

// SoulseekUpdate is the writable subset of SoulseekView. Enabled is
// deliberately absent — it is derived, not submitted.
type SoulseekUpdate struct {
	ServerAddress             string
	Username                  string
	Password                  *string
	ListenAddr                string
	UploadSlots               int
	AllowPrivatePeerAddresses bool
	Gluetun                   GluetunUpdate
	SharedFolders             []SharedFolderUpdate
}

type StoreUpdate struct {
	DSN *string
}

type ObservUpdate struct {
	ListenAddr string
	AuthToken  *string
	LogLevel   string
}

type PathsUpdate struct {
	SlskdCompleteDir string
}

// ConfigUpdate is the writable subset of configuration the settings view may
// change, decoded and validated from a POST /api/config request body.
type ConfigUpdate struct {
	Lidarr   LidarrUpdate
	Slskd    SlskdUpdate
	Pipeline PipelineUpdate
	Soulseek SoulseekUpdate
	Store    StoreUpdate
	Observ   ObservUpdate
	Paths    PathsUpdate
}

// ConfigWriter persists a validated ConfigUpdate to the config file.
// Implementations should return ErrConfigNotWritable when the underlying
// file cannot be written, so the handler can report a helpful 409 instead of
// a generic 500, and should return a *ConfigValidationError when the update
// was rejected by internal/config's backstop validator, so the handler can
// report a 422 with that safe-to-display message instead of a generic 500.
type ConfigWriter func(ConfigUpdate) error

// ErrConfigNotWritable is returned by a ConfigWriter when the config file
// cannot be written — e.g. it is mounted read-only, or as a single bind-
// mounted file rather than a writable directory. The POST handler reports it
// as 409 with this message so the user knows how to fix their mount.
var ErrConfigNotWritable = errors.New("config file is not writable; mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount")

// ConfigValidationError is returned by a ConfigWriter when internal/config's
// backstop validator rejected the rendered document before any disk write
// happened — typically a cross-field rule the per-field validation below
// does not duplicate (e.g. pipeline.backend = "soulseek" requiring a
// configured [soulseek] section). Message is internal/config's own
// validation message: safe to display verbatim, since it never embeds paths
// or secrets. The POST handler reports it as 422 with an empty fieldErrors
// map, since the problem cannot be pinned to one request field.
type ConfigValidationError struct {
	Message string
}

func (e *ConfigValidationError) Error() string { return e.Message }

// ConnectionTester actively probes external dependencies for the settings
// view's "test connections" buttons. Each probe MUST use the loaded server-side
// configuration only — never values from the request — so the endpoints cannot
// be turned into an arbitrary outbound HTTP client. A nil field means the
// dependency cannot be tested in the running configuration (e.g. Soulseek
// disabled); its endpoint then reports that rather than failing opaquely.
type ConnectionTester struct {
	Lidarr   func(ctx context.Context) error
	Soulseek func(ctx context.Context) error
}

// connectionTestResult is the JSON shape served by the test endpoints. error is
// a human-readable reason on failure and never contains secrets.
type connectionTestResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// registerConfig wires the read/write config endpoint and the per-dependency
// connection tests onto mux.
func registerConfig(mux *http.ServeMux, config ConfigFunc, tester ConnectionTester, writer ConfigWriter, restart func()) {
	mux.Handle("/api/config", newConfigHandler(config, writer, restart))
	mux.Handle("/api/config/test/lidarr", newConnectionTestHandler("lidarr", tester.Lidarr))
	mux.Handle("/api/config/test/soulseek", newConnectionTestHandler("soulseek", tester.Soulseek))
}

// newConfigHandler serves the current config on GET and, on POST, validates
// and applies a ConfigUpdate before scheduling a restart to pick it up.
func newConfigHandler(config ConfigFunc, writer ConfigWriter, restart func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			serveConfigGet(w, config)
		case http.MethodPost:
			serveConfigPost(w, r, writer, restart)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func serveConfigGet(w http.ResponseWriter, config ConfigFunc) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(config())
}

// --- POST /api/config request body ---
//
// Each *UpdateRequest struct below mirrors AppConfig's shape (same nesting,
// same JSON field names) rather than the *Update conversion types above: the
// wire format is what the settings view actually posts, secrets included as
// optional pointers.

type lidarrUpdateRequest struct {
	URL    string  `json:"url"`
	APIKey *string `json:"apiKey"`
}

type slskdUpdateRequest struct {
	URL    string  `json:"url"`
	APIKey *string `json:"apiKey"`
}

type weightsUpdateRequest struct {
	Format      float64 `json:"format"`
	Bitrate     float64 `json:"bitrate"`
	Reliability float64 `json:"reliability"`
	FileCount   float64 `json:"fileCount"`
	KnownUser   float64 `json:"knownUser"`
}

type pipelineUpdateRequest struct {
	Backend               string               `json:"backend"`
	MaxCandidatesPerAlbum int                  `json:"maxCandidatesPerAlbum"`
	MaxActive             int                  `json:"maxActive"`
	MaxRetries            int                  `json:"maxRetries"`
	MaxInflightPerPeer    int                  `json:"maxInflightPerPeer"`
	MaxTransferRetries    int                  `json:"maxTransferRetries"`
	MinBitrate            int                  `json:"minBitrate"`
	TransferDeadline      string               `json:"transferDeadline"`
	StallTimeout          string               `json:"stallTimeout"`
	SearchTimeout         string               `json:"searchTimeout"`
	BackoffBase           string               `json:"backoffBase"`
	BackoffCap            string               `json:"backoffCap"`
	CandidateTTL          string               `json:"candidateTtl"`
	FailedReviveAfter     string               `json:"failedReviveAfter"`
	StuckAfter            string               `json:"stuckAfter"`
	TickTimeout           string               `json:"tickTimeout"`
	ImportConfirmTimeout  string               `json:"importConfirmTimeout"`
	WantedSyncInterval    string               `json:"wantedSyncInterval"`
	DiscoveryInterval     string               `json:"discoveryInterval"`
	SelectingInterval     string               `json:"selectingInterval"`
	DownloadingInterval   string               `json:"downloadingInterval"`
	ImportingInterval     string               `json:"importingInterval"`
	ManualImportTimeout   string               `json:"manualImportTimeout"`
	ImportRetryCooldown   string               `json:"importRetryCooldown"`
	Weights               weightsUpdateRequest `json:"weights"`
}

type gluetunUpdateRequest struct {
	ControlURL string  `json:"controlUrl"`
	APIKey     *string `json:"apiKey"`
}

type sharedFolderUpdateRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type soulseekUpdateRequest struct {
	ServerAddress             string                      `json:"serverAddress"`
	Username                  string                      `json:"username"`
	Password                  *string                     `json:"password"`
	ListenAddr                string                      `json:"listenAddr"`
	UploadSlots               int                         `json:"uploadSlots"`
	AllowPrivatePeerAddresses bool                        `json:"allowPrivatePeerAddresses"`
	Gluetun                   gluetunUpdateRequest        `json:"gluetun"`
	SharedFolders             []sharedFolderUpdateRequest `json:"sharedFolders"`
}

type storeUpdateRequest struct {
	DSN *string `json:"dsn"`
}

type observUpdateRequest struct {
	ListenAddr string  `json:"listenAddr"`
	AuthToken  *string `json:"authToken"`
	LogLevel   string  `json:"logLevel"`
}

type pathsUpdateRequest struct {
	SlskdCompleteDir string `json:"slskdCompleteDir"`
}

type configUpdateRequest struct {
	Lidarr   lidarrUpdateRequest   `json:"lidarr"`
	Slskd    slskdUpdateRequest    `json:"slskd"`
	Pipeline pipelineUpdateRequest `json:"pipeline"`
	Soulseek soulseekUpdateRequest `json:"soulseek"`
	Store    storeUpdateRequest    `json:"store"`
	Observ   observUpdateRequest   `json:"observ"`
	Paths    pathsUpdateRequest    `json:"paths"`
}

// errorResponse is the JSON error shape for every non-2xx /api/config
// response, and is reused by every other JSON endpoint's error body.
// FieldErrors is populated only for a 422 validation failure, keyed by a
// dotted JSON path (e.g. "pipeline.maxActive",
// "soulseek.sharedFolders[0].name") into the request body. Code is a stable
// machine-readable identifier for the small set of errors a caller needs to
// branch on programmatically rather than just display - currently only
// POST /api/lidarr/artists' "addUncertain" (issue #331 backend review), via
// writeConfigErrorWithCode; every other error leaves it empty.
type errorResponse struct {
	Error       string            `json:"error"`
	Code        string            `json:"code,omitempty"`
	FieldErrors map[string]string `json:"fieldErrors,omitempty"`
}

func writeConfigError(w http.ResponseWriter, status int, message string, fieldErrors map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: message, FieldErrors: fieldErrors})
}

// writeConfigErrorWithCode is writeConfigError plus a machine-readable code -
// see errorResponse's doc comment.
func writeConfigErrorWithCode(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: message, Code: code})
}

func serveConfigPost(w http.ResponseWriter, r *http.Request, writer ConfigWriter, restart func()) {
	var req configUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConfigError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	update, fieldErrors := validateConfigUpdate(req)
	if len(fieldErrors) > 0 {
		writeConfigError(w, http.StatusUnprocessableEntity, "validation failed", fieldErrors)
		return
	}

	if err := writer(update); err != nil {
		if errors.Is(err, ErrConfigNotWritable) {
			writeConfigError(w, http.StatusConflict, ErrConfigNotWritable.Error(), nil)
			return
		}
		var verr *ConfigValidationError
		if errors.As(err, &verr) {
			writeConfigError(w, http.StatusUnprocessableEntity, verr.Message, nil)
			return
		}
		// Never echo the underlying error to the client: it could embed
		// filesystem paths or, in principle, request values.
		writeConfigError(w, http.StatusInternalServerError, "failed to save configuration", nil)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		OK         bool `json:"ok"`
		Restarting bool `json:"restarting"`
	}{OK: true, Restarting: true})

	// Give the response time to flush to the client before the process exits.
	go func() {
		time.Sleep(250 * time.Millisecond)
		restart()
	}()
}

// secretOrKeep trims req and, if non-blank, returns a pointer to the trimmed
// value; otherwise nil ("keep the currently configured value"). An absent
// JSON field and an explicitly blank one are therefore indistinguishable —
// both mean keep, matching the API contract (the settings view never
// receives a configured secret back, so it has no way to resend one
// unchanged, and there is deliberately no way to blank out a secret via this
// form).
func secretOrKeep(req *string) *string {
	if req == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*req)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// observAuthTokenPlaceholder mirrors internal/config's unexported constant of
// the same name (config.toml.example's auth_token placeholder). Duplicated
// here, rather than imported, because observ deliberately does not depend on
// internal/config (see the package comment) — the string is small and
// unlikely to drift silently since both sides reject it as an actual token.
const observAuthTokenPlaceholder = "REPLACE_WITH_A_LONG_RANDOM_TOKEN"

// allowedLogLevels mirrors internal/config's log-level enum for the same
// reason observAuthTokenPlaceholder is duplicated above.
var allowedLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

// validateConfigUpdate checks every field before any I/O happens, collecting
// every problem (not just the first) so the settings view can show them all
// at once, keyed by a dotted JSON path into the request body.
//
// Only rules internal/config.Validate applies unconditionally to a single
// field are duplicated here (so the settings view gets immediate, precisely
// located feedback for them). Cross-field rules — e.g. slskd.url is only
// required when pipeline.backend = "slskd", soulseek.* presence is only
// required when the section ends up enabled — are deliberately left to
// internal/config's LoadBytes backstop, which ConfigWriter surfaces as a 422
// with an empty fieldErrors map (see ConfigValidationError). observ.authToken
// has no cross-field rule at all since issue #279: it is unconditionally
// optional now that browser access goes through form-based session login
// instead.
func validateConfigUpdate(req configUpdateRequest) (ConfigUpdate, map[string]string) {
	fieldErrors := make(map[string]string)
	var update ConfigUpdate

	// --- lidarr ---
	if u, err := url.Parse(req.Lidarr.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fieldErrors["lidarr.url"] = "must be an absolute http(s) URL"
	} else {
		update.Lidarr.URL = req.Lidarr.URL
	}
	if key := secretOrKeep(req.Lidarr.APIKey); key != nil {
		if strings.ContainsAny(*key, " \t\r\n") {
			fieldErrors["lidarr.apiKey"] = "must not contain embedded whitespace"
		} else {
			update.Lidarr.APIKey = key
		}
	}

	// --- slskd --- (url format checked only when non-blank: it is only
	// required when pipeline.backend = "slskd", a cross-field rule left to
	// the backstop)
	if req.Slskd.URL != "" {
		if u, err := url.Parse(req.Slskd.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fieldErrors["slskd.url"] = "must be an absolute http(s) URL"
		} else {
			update.Slskd.URL = req.Slskd.URL
		}
	}
	if key := secretOrKeep(req.Slskd.APIKey); key != nil {
		if strings.ContainsAny(*key, " \t\r\n") {
			fieldErrors["slskd.apiKey"] = "must not contain embedded whitespace"
		} else {
			update.Slskd.APIKey = key
		}
	}

	validatePipeline(req.Pipeline, &update.Pipeline, fieldErrors)
	validateSoulseek(req.Soulseek, &update.Soulseek, fieldErrors)

	update.Store.DSN = secretOrKeep(req.Store.DSN)

	// --- observ ---
	if req.Observ.ListenAddr == "" {
		fieldErrors["observ.listenAddr"] = "is required"
	} else if _, _, err := net.SplitHostPort(req.Observ.ListenAddr); err != nil {
		fieldErrors["observ.listenAddr"] = "must be a valid host:port"
	} else {
		update.Observ.ListenAddr = req.Observ.ListenAddr
	}
	if token := secretOrKeep(req.Observ.AuthToken); token != nil {
		switch {
		case strings.ContainsAny(*token, " \t\r\n"):
			fieldErrors["observ.authToken"] = "must not contain whitespace"
		case *token == observAuthTokenPlaceholder:
			fieldErrors["observ.authToken"] = "must be replaced with a generated token"
		default:
			update.Observ.AuthToken = token
		}
	}
	if req.Observ.LogLevel != "" && !allowedLogLevels[strings.ToLower(req.Observ.LogLevel)] {
		fieldErrors["observ.logLevel"] = "must be one of debug, info, warn, error"
	} else {
		update.Observ.LogLevel = req.Observ.LogLevel
	}

	// --- paths ---
	if req.Paths.SlskdCompleteDir == "" {
		fieldErrors["paths.slskdCompleteDir"] = "is required"
	} else {
		update.Paths.SlskdCompleteDir = req.Paths.SlskdCompleteDir
	}

	return update, fieldErrors
}

func validatePipeline(req pipelineUpdateRequest, update *PipelineUpdate, fieldErrors map[string]string) {
	if req.Backend != "slskd" && req.Backend != "soulseek" {
		fieldErrors["pipeline.backend"] = `must be "slskd" or "soulseek"`
	} else {
		update.Backend = req.Backend
	}

	validateBoundedInt(req.MaxCandidatesPerAlbum, 1, "pipeline.maxCandidatesPerAlbum", "must be >= 1", fieldErrors, &update.MaxCandidatesPerAlbum)
	validateBoundedInt(req.MaxActive, 1, "pipeline.maxActive", "must be >= 1", fieldErrors, &update.MaxActive)
	validateBoundedInt(req.MaxRetries, 1, "pipeline.maxRetries", "must be >= 1", fieldErrors, &update.MaxRetries)
	validateBoundedInt(req.MaxInflightPerPeer, 1, "pipeline.maxInflightPerPeer", "must be >= 1", fieldErrors, &update.MaxInflightPerPeer)
	validateBoundedInt(req.MaxTransferRetries, 0, "pipeline.maxTransferRetries", "must be >= 0", fieldErrors, &update.MaxTransferRetries)
	validateBoundedInt(req.MinBitrate, 1, "pipeline.minBitrate", "must be >= 1", fieldErrors, &update.MinBitrate)

	validatePositiveDuration(req.TransferDeadline, "pipeline.transferDeadline", fieldErrors, &update.TransferDeadline)
	validatePositiveDuration(req.StallTimeout, "pipeline.stallTimeout", fieldErrors, &update.StallTimeout)
	validatePositiveDuration(req.SearchTimeout, "pipeline.searchTimeout", fieldErrors, &update.SearchTimeout)
	validatePositiveDuration(req.BackoffBase, "pipeline.backoffBase", fieldErrors, &update.BackoffBase)
	validatePositiveDuration(req.BackoffCap, "pipeline.backoffCap", fieldErrors, &update.BackoffCap)
	validatePositiveDuration(req.CandidateTTL, "pipeline.candidateTtl", fieldErrors, &update.CandidateTTL)
	validatePositiveDuration(req.FailedReviveAfter, "pipeline.failedReviveAfter", fieldErrors, &update.FailedReviveAfter)
	validatePositiveDuration(req.StuckAfter, "pipeline.stuckAfter", fieldErrors, &update.StuckAfter)
	validatePositiveDuration(req.TickTimeout, "pipeline.tickTimeout", fieldErrors, &update.TickTimeout)
	validatePositiveDuration(req.ImportConfirmTimeout, "pipeline.importConfirmTimeout", fieldErrors, &update.ImportConfirmTimeout)
	validatePositiveDuration(req.WantedSyncInterval, "pipeline.wantedSyncInterval", fieldErrors, &update.WantedSyncInterval)
	validatePositiveDuration(req.DiscoveryInterval, "pipeline.discoveryInterval", fieldErrors, &update.DiscoveryInterval)
	validatePositiveDuration(req.SelectingInterval, "pipeline.selectingInterval", fieldErrors, &update.SelectingInterval)
	validatePositiveDuration(req.DownloadingInterval, "pipeline.downloadingInterval", fieldErrors, &update.DownloadingInterval)
	validatePositiveDuration(req.ImportingInterval, "pipeline.importingInterval", fieldErrors, &update.ImportingInterval)
	validatePositiveDuration(req.ManualImportTimeout, "pipeline.manualImportTimeout", fieldErrors, &update.ManualImportTimeout)
	validatePositiveDuration(req.ImportRetryCooldown, "pipeline.importRetryCooldown", fieldErrors, &update.ImportRetryCooldown)

	// The other four weights have no bound in config.Validate; pass them
	// through as-is (JSON decoding already guarantees they are numeric).
	update.Weights.Format = req.Weights.Format
	update.Weights.Bitrate = req.Weights.Bitrate
	update.Weights.Reliability = req.Weights.Reliability
	update.Weights.FileCount = req.Weights.FileCount
	if req.Weights.KnownUser < 0 {
		fieldErrors["pipeline.weights.knownUser"] = "must be >= 0"
	} else {
		update.Weights.KnownUser = req.Weights.KnownUser
	}
}

func validateSoulseek(req soulseekUpdateRequest, update *SoulseekUpdate, fieldErrors map[string]string) {
	// serverAddress, username, listenAddr, and upload_slots' ">0 when
	// enabled" bound are cross-field (only required once the section is
	// enabled by some other field) and are left to the backstop; only the
	// *format* of a non-blank value is checked here.
	if req.ServerAddress != "" {
		if _, _, err := net.SplitHostPort(req.ServerAddress); err != nil {
			fieldErrors["soulseek.serverAddress"] = "must be a valid host:port"
		} else {
			update.ServerAddress = req.ServerAddress
		}
	}
	update.Username = req.Username
	update.Password = secretOrKeep(req.Password)
	if req.ListenAddr != "" {
		if _, port, err := net.SplitHostPort(req.ListenAddr); err != nil {
			fieldErrors["soulseek.listenAddr"] = "must be a valid host:port"
		} else if !validPort(port) {
			fieldErrors["soulseek.listenAddr"] = "port must be between 1 and 65535"
		} else {
			update.ListenAddr = req.ListenAddr
		}
	}
	if req.UploadSlots < 0 {
		fieldErrors["soulseek.uploadSlots"] = "must be >= 0"
	} else {
		update.UploadSlots = req.UploadSlots
	}
	update.AllowPrivatePeerAddresses = req.AllowPrivatePeerAddresses

	if req.Gluetun.ControlURL != "" {
		if u, err := url.Parse(req.Gluetun.ControlURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fieldErrors["soulseek.gluetun.controlUrl"] = "must be an http(s) URL"
		} else {
			update.Gluetun.ControlURL = req.Gluetun.ControlURL
		}
	}
	update.Gluetun.APIKey = secretOrKeep(req.Gluetun.APIKey)

	update.SharedFolders = make([]SharedFolderUpdate, len(req.SharedFolders))
	for i, f := range req.SharedFolders {
		name := strings.TrimSpace(f.Name)
		switch {
		case name != f.Name:
			fieldErrors[sharedFolderKey(i, "name")] = "must not contain surrounding whitespace"
		case name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`):
			fieldErrors[sharedFolderKey(i, "name")] = "must be nonblank and contain no path separators"
		}
		path := strings.TrimSpace(f.Path)
		switch {
		case path != f.Path:
			fieldErrors[sharedFolderKey(i, "path")] = "must not contain surrounding whitespace"
		case path == "" || !filepath.IsAbs(path):
			fieldErrors[sharedFolderKey(i, "path")] = "must be an absolute path"
		}
		update.SharedFolders[i] = SharedFolderUpdate{Name: f.Name, Path: f.Path}
	}
}

func sharedFolderKey(i int, leaf string) string {
	return "soulseek.sharedFolders[" + strconv.Itoa(i) + "]." + leaf
}

// validPort reports whether port is a numeric string in the valid TCP port
// range 1-65535. net.SplitHostPort only checks the address shape, not that
// the port is numeric: "0.0.0.0:abc" passes it and would otherwise only fail
// much later, at bind time.
func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func validateBoundedInt(value, min int, key, message string, fieldErrors map[string]string, dest *int) {
	if value < min {
		fieldErrors[key] = message
	} else {
		*dest = value
	}
}

func validatePositiveDuration(raw, key string, fieldErrors map[string]string, dest *time.Duration) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fieldErrors[key] = `must be a positive duration such as "5m"`
	} else {
		*dest = d
	}
}

// newConnectionTestHandler runs probe against the loaded configuration and
// reports {ok,error}. A nil probe means the dependency is not enabled in this
// configuration and yields a readable message instead of a misleading failure.
func newConnectionTestHandler(name string, probe func(ctx context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result := connectionTestResult{OK: true}
		switch {
		case probe == nil:
			result = connectionTestResult{Error: name + " is not enabled in the configuration"}
		default:
			if err := probe(r.Context()); err != nil {
				result = connectionTestResult{Error: err.Error()}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}
