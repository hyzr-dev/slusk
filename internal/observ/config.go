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
type AppConfig struct {
	LidarrURL              string `json:"lidarrUrl"`
	ReconcileInterval      string `json:"reconcileInterval"`
	MaxConcurrentDownloads int    `json:"maxConcurrentDownloads"`

	lidarrAPIKey string
}

// NewAppConfig builds the display config, keeping the API key out of the
// marshalled surface.
func NewAppConfig(lidarrURL, lidarrAPIKey, reconcileInterval string, maxConcurrent int) AppConfig {
	return AppConfig{
		LidarrURL:              lidarrURL,
		ReconcileInterval:      reconcileInterval,
		MaxConcurrentDownloads: maxConcurrent,
		lidarrAPIKey:           lidarrAPIKey,
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
