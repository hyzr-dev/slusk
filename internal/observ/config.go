// Package observ: config.go serves the settings view's read-only view of the
// running configuration and its connection tests. Secrets are never sent — only
// whether they are set, and connection tests probe the loaded config server-
// side rather than any client-supplied values. Writable configuration is
// deliberately out of scope; see issue #89.
package observ

import (
	"context"
	"encoding/json"
	"net/http"
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
	// SoulseekEnabled lets the settings view decide whether to offer a Soulseek
	// connection test at all; there is nothing to test when the native client is
	// disabled.
	SoulseekEnabled bool `json:"soulseekEnabled"`

	lidarrAPIKey string
}

// NewAppConfig builds the display config, keeping the API key out of the
// marshalled surface.
func NewAppConfig(lidarrURL, lidarrAPIKey, wantedSyncInterval string, maxActive int, soulseekEnabled bool) AppConfig {
	return AppConfig{
		LidarrURL:          lidarrURL,
		WantedSyncInterval: wantedSyncInterval,
		MaxActive:          maxActive,
		SoulseekEnabled:    soulseekEnabled,
		lidarrAPIKey:       lidarrAPIKey,
	}
}

// ConfigFunc produces the current display configuration.
type ConfigFunc func() AppConfig

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

// registerConfig wires the read-only config endpoint and the per-dependency
// connection tests onto mux.
func registerConfig(mux *http.ServeMux, config ConfigFunc, tester ConnectionTester) {
	mux.Handle("/api/config", newConfigHandler(config))
	mux.Handle("/api/config/test/lidarr", newConnectionTestHandler("lidarr", tester.Lidarr))
	mux.Handle("/api/config/test/soulseek", newConnectionTestHandler("soulseek", tester.Soulseek))
}

func newConfigHandler(config ConfigFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := config()
		resp := struct {
			AppConfig
			LidarrAPIKeyConfigured bool `json:"lidarrApiKeyConfigured"`
		}{c, c.lidarrAPIKey != ""}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
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
