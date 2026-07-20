// Package observ: config.go serves the settings view's read-only view of the
// running configuration. Secrets are never sent — only whether they are set.
// Writable configuration is deliberately out of scope; see issue #89.
package observ

import (
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

	lidarrAPIKey string
}

// NewAppConfig builds the display config, keeping the API key out of the
// marshalled surface.
func NewAppConfig(lidarrURL, lidarrAPIKey, wantedSyncInterval string, maxActive int) AppConfig {
	return AppConfig{
		LidarrURL:          lidarrURL,
		WantedSyncInterval: wantedSyncInterval,
		MaxActive:          maxActive,
		lidarrAPIKey:       lidarrAPIKey,
	}
}

// ConfigFunc produces the current display configuration.
type ConfigFunc func() AppConfig

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
