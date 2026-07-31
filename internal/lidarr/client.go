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

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// Client talks to a Lidarr instance.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	// scanHTTP is used only by ManualImportCandidates: Lidarr parses audio
	// tags per file during a manualimport folder scan, so large folders (box
	// sets, deluxe editions) legitimately take far longer than any other API
	// call and need their own, longer timeout.
	scanHTTP *http.Client
}

// Option configures a Client at construction.
type Option func(*Client)

// WithManualImportTimeout overrides how long a manualimport folder scan may
// take before the client gives up (default 10m). Values <= 0 are ignored.
func WithManualImportTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.scanHTTP.Timeout = d
		}
	}
}

// New constructs a Lidarr client.
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:  baseURL,
		apiKey:   apiKey,
		http:     &http.Client{Timeout: 30 * time.Second},
		scanHTTP: &http.Client{Timeout: 10 * time.Minute},
	}
	for _, o := range opts {
		o(c)
	}
	return c
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
func (c *Client) WantedMissing(ctx context.Context) ([]core.WantedRelease, error) {
	var out []core.WantedRelease
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
			out = append(out, core.WantedRelease{ID: r.ID, Title: r.Title, ArtistName: r.Artist.ArtistName, ArtistID: r.Artist.ID, ReleaseDate: r.ReleaseDate})
		}
		if len(out) >= body.TotalRecords {
			break
		}
	}
	return out, nil
}

// Ping verifies the configured URL and API key by requesting Lidarr's system
// status, for the settings view's connection test. It returns nil when Lidarr
// answers 2xx; a 401 is reported distinctly (wrong API key) since that is the
// most common misconfiguration, and any other non-2xx or transport error is
// returned verbatim. The response body is discarded — only reachability and
// authorization matter here.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/system/status", c.baseURL), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach Lidarr: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("lidarr rejected the API key (status 401)")
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lidarr system/status: status %d", resp.StatusCode)
	}
	return nil
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

// ManualImportCandidates asks Lidarr what it would import from folder, including
// each file's rejection reasons (empty rejections => importable).
func (c *Client) ManualImportCandidates(ctx context.Context, folder string) ([]core.ImportItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/manualimport?folder="+url.QueryEscape(folder), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.scanHTTP.Do(req)
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
	out := make([]core.ImportItem, 0, len(raw))
	for _, it := range raw {
		reasons := make([]string, 0, len(it.Rejections))
		for _, r := range it.Rejections {
			reasons = append(reasons, r.Reason)
		}
		trackIDs := make([]int64, 0, len(it.Tracks))
		for _, tr := range it.Tracks {
			trackIDs = append(trackIDs, tr.ID)
		}
		out = append(out, core.ImportItem{
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

// AlbumReleases lists every release of an album, used by discovery to compute
// the valid track-count band [min, max] across all editions rather than
// filtering against the single canonical count. Lidarr v3 removed the
// standalone /albumrelease endpoint; releases are read from the album
// resource's embedded "releases" array instead.
func (c *Client) AlbumReleases(ctx context.Context, albumID int64) ([]core.AlbumRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/album/%d", c.baseURL, albumID), nil)
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
		return nil, fmt.Errorf("lidarr album releases: status %d", resp.StatusCode)
	}
	var body struct {
		Releases []struct {
			ID         int64 `json:"id"`
			TrackCount int   `json:"trackCount"`
			Monitored  bool  `json:"monitored"`
		} `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]core.AlbumRelease, 0, len(body.Releases))
	for _, r := range body.Releases {
		out = append(out, core.AlbumRelease{ID: r.ID, TrackCount: r.TrackCount, Monitored: r.Monitored})
	}
	return out, nil
}

// AlbumTracks fetches an album's tracklist, used by the discovery relevance
// gate (#316) to check candidate filenames against the album's real track
// titles rather than trusting Soulseek's token-AND path match alone.
//
// The response shape assumed here — a flat JSON array of TrackResource, each
// carrying at least "title" — and whether it covers every release of the
// album or only the one Lidarr currently has selected, are both unverified
// against the deployed Lidarr version. A failure here degrades the caller to
// a directory-only relevance check rather than aborting discovery (see
// discovery.go), so a wrong assumption here is not a hard outage. Only
// "title" is decoded (see core.AlbumTrack) - other fields' types are
// therefore free to drift between Lidarr versions without breaking this call.
func (c *Client) AlbumTracks(ctx context.Context, albumID int64) ([]core.AlbumTrack, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/track?albumId=%d", c.baseURL, albumID), nil)
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
		return nil, fmt.Errorf("lidarr album tracks: status %d", resp.StatusCode)
	}
	var raw []struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]core.AlbumTrack, 0, len(raw))
	for _, r := range raw {
		out = append(out, core.AlbumTrack{Title: r.Title})
	}
	return out, nil
}

// AlbumByForeignID reports whether a MusicBrainz release-group is in the
// user's Lidarr library, keyed by Lidarr's foreignAlbumId - which is exactly
// the release-group's MusicBrainz id (issue #321). /api/v1/album/lookup is
// the wrong endpoint for this: it queries Lidarr's metadata server, not the
// library, so it would report "found" for any album that exists anywhere in
// MusicBrainz rather than only the ones the user has actually added.
//
// found is false, err is nil when Lidarr answered with an empty result -  a
// genuine "not in library". A non-nil err means the answer is unknown (Lidarr
// unreachable or erroring), which callers must not treat as absence - see
// app.LidarrAlbumStatus.
func (c *Client) AlbumByForeignID(ctx context.Context, foreignAlbumID string) (core.LidarrAlbum, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/album?foreignAlbumId=%s", c.baseURL, url.QueryEscape(foreignAlbumID)), nil)
	if err != nil {
		return core.LidarrAlbum{}, false, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return core.LidarrAlbum{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return core.LidarrAlbum{}, false, fmt.Errorf("lidarr album lookup: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID        int64 `json:"id"`
		ArtistID  int64 `json:"artistId"`
		Monitored bool  `json:"monitored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return core.LidarrAlbum{}, false, err
	}
	if len(raw) == 0 {
		return core.LidarrAlbum{}, false, nil
	}
	a := raw[0]
	return core.LidarrAlbum{ID: a.ID, ArtistID: a.ArtistID, Monitored: a.Monitored}, true, nil
}

// ExecuteManualImport tells Lidarr to import the given items (move mode).
func (c *Client) ExecuteManualImport(ctx context.Context, items []core.ImportItem) error {
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
