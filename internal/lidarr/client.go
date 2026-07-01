// Package lidarr is a thin REST client for Lidarr. It mirrors Lidarr's API and
// returns its own types; it knows nothing about slskdarr's database.
package lidarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client talks to a Lidarr instance.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a Lidarr client.
func New(baseURL, apiKey string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
}

// WantedAlbum is one wanted/missing album from Lidarr.
type WantedAlbum struct {
	ID         int64
	Title      string
	ArtistName string
}

// WantedMissing fetches the wanted/missing album records.
func (c *Client) WantedMissing(ctx context.Context) ([]WantedAlbum, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/wanted/missing?pageSize=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lidarr wanted/missing: status %d", resp.StatusCode)
	}
	var body struct {
		Records []struct {
			ID     int64  `json:"id"`
			Title  string `json:"title"`
			Artist struct {
				ArtistName string `json:"artistName"`
			} `json:"artist"`
		} `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]WantedAlbum, 0, len(body.Records))
	for _, r := range body.Records {
		out = append(out, WantedAlbum{ID: r.ID, Title: r.Title, ArtistName: r.Artist.ArtistName})
	}
	return out, nil
}

// TriggerImport asks Lidarr to scan and import the downloaded folder at path.
func (c *Client) TriggerImport(ctx context.Context, path string) error {
	body := map[string]any{"name": "DownloadedAlbumsScan", "path": path}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/command", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lidarr command: status %d", resp.StatusCode)
	}
	return nil
}
