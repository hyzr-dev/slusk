// Package observ: config.go serves the settings view's view of the running
// configuration, its connection tests, and (see issue #89) the writable
// settings update endpoint. Secrets are never sent — only whether they are
// set, and connection tests probe the loaded config server-side rather than
// any client-supplied values. Updates take effect by restarting the process
// so the whole application always runs off one freshly-loaded config.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AppConfig is the subset of configuration the settings view displays. The
// Lidarr API key is unexported so it can never be marshalled by accident.
//
// Field names mirror the [pipeline] TOML keys they come from
// (WantedSyncInterval <-> wanted_sync_interval, MaxActive <-> max_active) so
// the settings view can display something that matches the config file the
// user actually edits.
type AppConfig struct {
	LidarrURL          string `json:"lidarrUrl"`
	WantedSyncInterval string `json:"wantedSyncInterval"`
	MaxActive          int    `json:"maxActive"`
	MinBitrate         int    `json:"minBitrate"`
	StallTimeout       string `json:"stallTimeout"`
	// SoulseekEnabled lets the settings view decide whether to offer a Soulseek
	// connection test at all; there is nothing to test when the native client is
	// disabled.
	SoulseekEnabled bool `json:"soulseekEnabled"`
	// Writable reports whether the config file's directory currently accepts
	// writes (see config.ProbeWritable), so the settings view can render an
	// editable form or a read-only one without first attempting a write.
	Writable bool `json:"writable"`

	lidarrAPIKey string
}

// NewAppConfig builds the display config, keeping the API key out of the
// marshalled surface.
func NewAppConfig(lidarrURL, lidarrAPIKey, wantedSyncInterval string, maxActive, minBitrate int, stallTimeout string, soulseekEnabled, writable bool) AppConfig {
	return AppConfig{
		LidarrURL:          lidarrURL,
		WantedSyncInterval: wantedSyncInterval,
		MaxActive:          maxActive,
		MinBitrate:         minBitrate,
		StallTimeout:       stallTimeout,
		SoulseekEnabled:    soulseekEnabled,
		Writable:           writable,
		lidarrAPIKey:       lidarrAPIKey,
	}
}

// ConfigFunc produces the current display configuration.
type ConfigFunc func() AppConfig

// ConfigUpdate is the writable subset of configuration the settings view may
// change, decoded and validated from a POST /api/config request body. A nil
// LidarrAPIKey means "keep the currently configured value" — the settings
// view never receives the secret back, so it has no way to resend it
// unchanged. observ deliberately does not import internal/config (see the
// package comment); main wires a ConfigWriter that converts this into
// config.Settings.
type ConfigUpdate struct {
	LidarrURL          string
	LidarrAPIKey       *string
	WantedSyncInterval time.Duration
	StallTimeout       time.Duration
	MaxActive          int
	MinBitrate         int
}

// ConfigWriter persists a validated ConfigUpdate to the config file.
// Implementations should return ErrConfigNotWritable when the underlying
// file cannot be written, so the handler can report a helpful 409 instead of
// a generic 500.
type ConfigWriter func(ConfigUpdate) error

// ErrConfigNotWritable is returned by a ConfigWriter when the config file
// cannot be written — e.g. it is mounted read-only, or as a single bind-
// mounted file rather than a writable directory. The POST handler reports it
// as 409 with this message so the user knows how to fix their mount.
var ErrConfigNotWritable = errors.New("config file is not writable; mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount")

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
	c := config()
	resp := struct {
		AppConfig
		LidarrAPIKeyConfigured bool `json:"lidarrApiKeyConfigured"`
	}{c, c.lidarrAPIKey != ""}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// configUpdateRequest is the POST /api/config request body. LidarrAPIKey is a
// pointer so an absent field and an explicitly blank one are both
// distinguishable from a real key, and both mean "keep the current value".
type configUpdateRequest struct {
	LidarrURL          string  `json:"lidarrUrl"`
	LidarrAPIKey       *string `json:"lidarrApiKey"`
	WantedSyncInterval string  `json:"wantedSyncInterval"`
	StallTimeout       string  `json:"stallTimeout"`
	MaxActive          int     `json:"maxActive"`
	MinBitrate         int     `json:"minBitrate"`
}

// errorResponse is the JSON error shape for every non-2xx /api/config
// response. FieldErrors is populated only for 422 validation failures, keyed
// by the request field name.
type errorResponse struct {
	Error       string            `json:"error"`
	FieldErrors map[string]string `json:"fieldErrors,omitempty"`
}

func writeConfigError(w http.ResponseWriter, status int, message string, fieldErrors map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: message, FieldErrors: fieldErrors})
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

// validateConfigUpdate checks every field before any I/O happens, collecting
// every problem (not just the first) so the settings view can show them all
// at once, keyed by the request field name they came from.
func validateConfigUpdate(req configUpdateRequest) (ConfigUpdate, map[string]string) {
	fieldErrors := make(map[string]string)
	var update ConfigUpdate

	if u, err := url.Parse(req.LidarrURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fieldErrors["lidarrUrl"] = "must be an absolute http(s) URL"
	} else {
		update.LidarrURL = req.LidarrURL
	}

	if req.LidarrAPIKey != nil {
		key := strings.TrimSpace(*req.LidarrAPIKey)
		if key != "" {
			if strings.ContainsAny(key, " \t\r\n") {
				fieldErrors["lidarrApiKey"] = "must not contain embedded whitespace"
			} else {
				update.LidarrAPIKey = &key
			}
		}
		// An absent or blank key means "keep the current value" (update.LidarrAPIKey stays nil).
	}

	if d, err := time.ParseDuration(req.WantedSyncInterval); err != nil || d <= 0 {
		fieldErrors["wantedSyncInterval"] = `must be a positive duration such as "5m"`
	} else {
		update.WantedSyncInterval = d
	}

	if d, err := time.ParseDuration(req.StallTimeout); err != nil || d <= 0 {
		fieldErrors["stallTimeout"] = `must be a positive duration such as "5m"`
	} else {
		update.StallTimeout = d
	}

	if req.MaxActive < 1 {
		fieldErrors["maxActive"] = "must be >= 1"
	} else {
		update.MaxActive = req.MaxActive
	}

	if req.MinBitrate < 1 {
		fieldErrors["minBitrate"] = "must be >= 1"
	} else {
		update.MinBitrate = req.MinBitrate
	}

	return update, fieldErrors
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
