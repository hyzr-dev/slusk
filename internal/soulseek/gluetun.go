package soulseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// fetchGluetunPort queries the gluetun control server configured via
// c.cfg.GluetunControlURL for the currently forwarded port. It performs a
// single request bounded by c.cfg.gluetunTimeout - retry and backoff are
// retryStartup's job (which also warn-logs every failed attempt), and
// trySetup logs the fetched port on success, so this function does neither
// on its own.
func (c *Client) fetchGluetunPort(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.gluetunTimeout)
	defer cancel()

	url := strings.TrimSuffix(c.cfg.GluetunControlURL, "/") + "/v1/portforward"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build gluetun request: %w", err)
	}
	if c.cfg.GluetunAPIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.GluetunAPIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch gluetun forwarded port: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return 0, fmt.Errorf("gluetun control server rejected the request (HTTP %d): check soulseek.gluetun.api_key", resp.StatusCode)
		}
		return 0, fmt.Errorf("gluetun control server returned HTTP %d", resp.StatusCode)
	}

	var body struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode gluetun forwarded port response: %w", err)
	}

	if body.Port == 0 {
		return 0, fmt.Errorf("gluetun reports forwarded port 0: VPN port forwarding not yet established")
	}
	if body.Port < 0 || body.Port > 65535 {
		return 0, fmt.Errorf("gluetun reports out-of-range forwarded port %d", body.Port)
	}

	return body.Port, nil
}
