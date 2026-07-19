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
	// ArtistID is Lidarr's artist id, cached onto AlbumJob so peer reliability
	// history (artist_user_reliability) can be keyed by artist rather than by
	// artist name, which can be renamed.
	ArtistID int64
	// ReleaseDate is Lidarr's raw release date/datetime string for the album.
	ReleaseDate string
}

// wantedMissingPage is one page of Lidarr's wanted/missing response.
type wantedMissingPage struct {
	Page         int `json:"page"`
	PageSize     int `json:"pageSize"`
	TotalRecords int `json:"totalRecords"`
	Records      []struct {
		ID          int64  `json:"id"`
		Title       string `json:"title"`
		ReleaseDate string `json:"releaseDate"`
		Artist      struct {
			ID         int64  `json:"id"`
			ArtistName string `json:"artistName"`
		} `json:"artist"`
	} `json:"records"`
}

// fetchWantedMissingPage fetches a single page of wanted/missing records.
func (c *Client) fetchWantedMissingPage(ctx context.Context, page int) (wantedMissingPage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/wanted/missing?page=%d&pageSize=100", c.baseURL, page), nil)
	if err != nil {
		return wantedMissingPage{}, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return wantedMissingPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return wantedMissingPage{}, fmt.Errorf("lidarr wanted/missing: status %d", resp.StatusCode)
	}
	var body wantedMissingPage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return wantedMissingPage{}, err
	}
	return body, nil
}

// WantedMissing fetches every wanted/missing album record, paging through
// Lidarr's results until all totalRecords have been collected.
func (c *Client) WantedMissing(ctx context.Context) ([]WantedAlbum, error) {
	var out []WantedAlbum
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, err := c.fetchWantedMissingPage(ctx, page)
		if err != nil {
			return nil, err
		}
		if len(body.Records) == 0 {
			break
		}
		for _, r := range body.Records {
			out = append(out, WantedAlbum{ID: r.ID, Title: r.Title, ArtistName: r.Artist.ArtistName, ArtistID: r.Artist.ID, ReleaseDate: r.ReleaseDate})
		}
		if len(out) >= body.TotalRecords {
			break
		}
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
	ID                      int64
	Path                    string
	ArtistID                int64
	AlbumID                 int64
	AlbumReleaseID          int64
	TrackIDs                []int64
	Quality                 json.RawMessage // echoed back to Lidarr as-is on import
	IndexerFlags            int64
	DisableReleaseSwitching bool
	Rejections              []string
	Importable              bool // true when Rejections is empty
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
		ID     int64  `json:"id"`
		Path   string `json:"path"`
		Artist struct {
			ID int64 `json:"id"`
		} `json:"artist"`
		Album struct {
			ID int64 `json:"id"`
		} `json:"album"`
		AlbumReleaseID          int64           `json:"albumReleaseId"`
		Quality                 json.RawMessage `json:"quality"`
		IndexerFlags            int64           `json:"indexerFlags"`
		DisableReleaseSwitching bool            `json:"disableReleaseSwitching"`
		Tracks                  []struct {
			ID int64 `json:"id"`
		} `json:"tracks"`
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
		trackIDs := make([]int64, 0, len(it.Tracks))
		for _, tr := range it.Tracks {
			trackIDs = append(trackIDs, tr.ID)
		}
		out = append(out, ManualImportItem{
			ID: it.ID, Path: it.Path,
			ArtistID: it.Artist.ID, AlbumID: it.Album.ID, AlbumReleaseID: it.AlbumReleaseID,
			TrackIDs: trackIDs, Quality: it.Quality, IndexerFlags: it.IndexerFlags,
			DisableReleaseSwitching: it.DisableReleaseSwitching,
			Rejections:              reasons, Importable: len(reasons) == 0,
		})
	}
	return out, nil
}

// AlbumStatus reports how many of an album's tracks Lidarr currently has a
// file for (present) out of the release's total track count (total), used to
// judge whether a candidate download can complete the release.
func (c *Client) AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/album/%d", c.baseURL, albumID), nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("lidarr album: status %d", resp.StatusCode)
	}
	var body struct {
		Statistics struct {
			TrackFileCount int `json:"trackFileCount"`
			TrackCount     int `json:"trackCount"`
		} `json:"statistics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, 0, err
	}
	return body.Statistics.TrackFileCount, body.Statistics.TrackCount, nil
}

// AlbumRelease is one release (edition/pressing) of an album in Lidarr, with
// its own track count. Different releases of the same album legitimately have
// different track counts (bonus tracks, deluxe editions), and any of them is a
// valid import target since manual import runs with release switching enabled.
type AlbumRelease struct {
	ID         int64
	TrackCount int
	Monitored  bool
}

// AlbumReleases lists every release of an album, used by discovery to compute
// the valid track-count band [min, max] across all editions rather than
// filtering against the single canonical count.
func (c *Client) AlbumReleases(ctx context.Context, albumID int64) ([]AlbumRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/albumrelease?albumId=%d", c.baseURL, albumID), nil)
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
		return nil, fmt.Errorf("lidarr albumrelease: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID         int64 `json:"id"`
		TrackCount int   `json:"trackCount"`
		Monitored  bool  `json:"monitored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]AlbumRelease, 0, len(raw))
	for _, r := range raw {
		out = append(out, AlbumRelease{ID: r.ID, TrackCount: r.TrackCount, Monitored: r.Monitored})
	}
	return out, nil
}

// ExecuteManualImport tells Lidarr to import the given items (move mode).
func (c *Client) ExecuteManualImport(ctx context.Context, items []ManualImportItem) error {
	files := make([]map[string]any, 0, len(items))
	for _, it := range items {
		files = append(files, map[string]any{
			"id":                      it.ID,
			"path":                    it.Path,
			"artistId":                it.ArtistID,
			"albumId":                 it.AlbumID,
			"albumReleaseId":          it.AlbumReleaseID,
			"trackIds":                it.TrackIDs,
			"quality":                 it.Quality,
			"indexerFlags":            it.IndexerFlags,
			"additionalFile":          false,
			"replaceExistingFiles":    true,
			"disableReleaseSwitching": false,
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
