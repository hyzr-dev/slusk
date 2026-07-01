// Package lidarr is a thin REST client for Lidarr. It mirrors Lidarr's API and
// returns its own types; it knows nothing about slskdarr's database.
package lidarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	TrackCount int
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
			Statistics struct {
				TrackCount int `json:"trackCount"`
			} `json:"statistics"`
		} `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]WantedAlbum, 0, len(body.Records))
	for _, r := range body.Records {
		out = append(out, WantedAlbum{ID: r.ID, Title: r.Title, ArtistName: r.Artist.ArtistName, TrackCount: r.Statistics.TrackCount})
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

// ManualImportItem is one file Lidarr found in a folder, with any import rejections.
type ManualImportItem struct {
	ID         int64
	Path       string
	FolderName string
	ArtistID   int64
	AlbumID    int64
	Rejections []string
	Importable bool // true when Rejections is empty
}

// ManualImportCandidates asks Lidarr what it would import from folder, including
// each file's rejection reasons (empty rejections => importable).
func (c *Client) ManualImportCandidates(ctx context.Context, folder string) ([]ManualImportItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/manualimport?folder="+url.QueryEscape(folder), nil)
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
		return nil, fmt.Errorf("lidarr manualimport: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		FolderName string `json:"folderName"`
		ArtistID   int64  `json:"artistId"`
		AlbumID    int64  `json:"albumId"`
		Rejections []struct {
			Reason string `json:"reason"`
		} `json:"rejections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]ManualImportItem, 0, len(raw))
	for _, it := range raw {
		reasons := make([]string, 0, len(it.Rejections))
		for _, r := range it.Rejections {
			reasons = append(reasons, r.Reason)
		}
		out = append(out, ManualImportItem{
			ID: it.ID, Path: it.Path, FolderName: it.FolderName,
			ArtistID: it.ArtistID, AlbumID: it.AlbumID,
			Rejections: reasons, Importable: len(reasons) == 0,
		})
	}
	return out, nil
}

// ExecuteManualImport tells Lidarr to import the given items (move mode).
func (c *Client) ExecuteManualImport(ctx context.Context, items []ManualImportItem) error {
	files := make([]map[string]any, 0, len(items))
	for _, it := range items {
		files = append(files, map[string]any{
			"path": it.Path, "folderName": it.FolderName,
			"artistId": it.ArtistID, "albumId": it.AlbumID,
		})
	}
	body := map[string]any{"name": "ManualImport", "importMode": "move", "files": files}
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
		return fmt.Errorf("lidarr ManualImport command: status %d", resp.StatusCode)
	}
	return nil
}
