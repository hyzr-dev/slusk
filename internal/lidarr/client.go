// Package lidarr is a thin REST client for Lidarr. It mirrors Lidarr's API and
// returns its own types; it knows nothing about slusk's database.
package lidarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
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
	// addArtistHTTP is used only by AddArtist. Verified against Lidarr
	// 3.1.0.4875 (.lidarr-endpoints-verified.md): POST /artist is bimodal -
	// ~0.08s once Lidarr has the artist's metadata cached, but over 30s (past
	// the shared client's timeout) on a cold metadata fetch, which is exactly
	// the first-time-add case this call exists for. The write still lands
	// server-side even when the client gives up waiting, so this also needs
	// to be long enough that a genuine timeout is rare rather than routine.
	addArtistHTTP *http.Client
	// addArtistRecheckAttempts/addArtistRecheckInterval bound AddArtist's
	// bounded retry against ArtistByMBID after a transport error on the
	// create request - a single immediate check cannot tell "not created"
	// from "not created *yet*" when Lidarr was merely slow. See AddArtist's
	// doc comment.
	addArtistRecheckAttempts int
	addArtistRecheckInterval time.Duration
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

// WithAddArtistTimeout overrides how long AddArtist's create request may take
// before the client gives up (default 5m). Values <= 0 are ignored.
func WithAddArtistTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.addArtistHTTP.Timeout = d
		}
	}
}

// WithAddArtistRecheck overrides how many times, and how far apart, AddArtist
// re-checks ArtistByMBID after a transport error on the create request
// (default 3 attempts, 2s apart). Values <= 0 leave the corresponding default
// in place; tests use this to avoid a real multi-second sleep.
func WithAddArtistRecheck(attempts int, interval time.Duration) Option {
	return func(c *Client) {
		if attempts > 0 {
			c.addArtistRecheckAttempts = attempts
		}
		if interval > 0 {
			c.addArtistRecheckInterval = interval
		}
	}
}

// New constructs a Lidarr client.
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:                  baseURL,
		apiKey:                   apiKey,
		http:                     &http.Client{Timeout: 30 * time.Second},
		scanHTTP:                 &http.Client{Timeout: 10 * time.Minute},
		addArtistHTTP:            &http.Client{Timeout: 5 * time.Minute},
		addArtistRecheckAttempts: 3,
		addArtistRecheckInterval: 2 * time.Second,
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
		// Lidarr's Rejection is { reason, type: "Permanent" | "Temporary" }
		// (src/NzbDrone.Core/DecisionEngine/Rejection.cs). The type is what
		// lets a job reach IMPORT_REFUSED instead of cycling through every
		// remaining candidate (issue #470). Note the constructor defaults to
		// Permanent, so a rejection built from a reason alone arrives here as
		// Permanent even when it describes something transient - see
		// core.ImportRejection.
		Rejections []struct {
			Reason string `json:"reason"`
			Type   string `json:"type"`
		} `json:"rejections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]core.ImportItem, 0, len(raw))
	for _, it := range raw {
		reasons := make([]core.ImportRejection, 0, len(it.Rejections))
		for _, r := range it.Rejections {
			// An absent or unrecognised type reads as permanent, matching
			// Lidarr's own constructor default. Only the explicit "Temporary"
			// buys a retry.
			//
			// EqualFold, not ==, and that is load-bearing: Lidarr serialises
			// the enum in lower case. A lab probe against
			// /api/v1/manualimport returned {"reason": "Couldn't find similar
			// album for [...]", "type": "permanent"}, so a case-sensitive
			// comparison against "Temporary" would read every genuinely
			// temporary rejection as permanent and end the job for good.
			reasons = append(reasons, core.ImportRejection{
				Reason:    r.Reason,
				Permanent: !strings.EqualFold(r.Type, "Temporary"),
			})
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

// ArtistByMBID reports whether a MusicBrainz artist is in the user's Lidarr
// library, keyed by Lidarr's foreignArtistId - the artist's MusicBrainz id
// (issue #331). Mirrors AlbumByForeignID's three-way contract exactly: found
// is false, err is nil only when Lidarr answered with an empty result - a
// genuine "not in library". A non-nil err means the answer is unknown
// (Lidarr unreachable or erroring), which callers must not treat as absence
// - see app.LidarrArtistStatus.
func (c *Client) ArtistByMBID(ctx context.Context, artistMBID string) (core.LidarrArtist, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/artist?mbId=%s", c.baseURL, url.QueryEscape(artistMBID)), nil)
	if err != nil {
		return core.LidarrArtist{}, false, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return core.LidarrArtist{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return core.LidarrArtist{}, false, fmt.Errorf("lidarr artist lookup: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID              int64  `json:"id"`
		ForeignArtistID string `json:"foreignArtistId"`
		ArtistName      string `json:"artistName"`
		Monitored       bool   `json:"monitored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return core.LidarrArtist{}, false, err
	}
	if len(raw) == 0 {
		return core.LidarrArtist{}, false, nil
	}
	a := raw[0]
	return core.LidarrArtist{ID: a.ID, ForeignArtistID: a.ForeignArtistID, Name: a.ArtistName, Monitored: a.Monitored}, true, nil
}

// RootFolders lists Lidarr's configured root folders (issue #331), each
// carrying accessible/freeSpace and the per-folder default profile ids that
// Lidarr's own "add artist" UI prefills its selectors from.
func (c *Client) RootFolders(ctx context.Context) ([]core.LidarrRootFolder, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/rootfolder", nil)
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
		return nil, fmt.Errorf("lidarr root folders: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID                       int64  `json:"id"`
		Path                     string `json:"path"`
		Accessible               bool   `json:"accessible"`
		FreeSpace                int64  `json:"freeSpace"`
		DefaultQualityProfileID  int64  `json:"defaultQualityProfileId"`
		DefaultMetadataProfileID int64  `json:"defaultMetadataProfileId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]core.LidarrRootFolder, 0, len(raw))
	for _, r := range raw {
		out = append(out, core.LidarrRootFolder{
			ID: r.ID, Path: r.Path, Accessible: r.Accessible, FreeSpace: r.FreeSpace,
			DefaultQualityProfileID: r.DefaultQualityProfileID, DefaultMetadataProfileID: r.DefaultMetadataProfileID,
		})
	}
	return out, nil
}

// fetchProfiles is the shared body of QualityProfiles/MetadataProfiles: both
// endpoints return the same {id, name} shape (issue #331).
func (c *Client) fetchProfiles(ctx context.Context, path string) ([]core.LidarrProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
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
		return nil, fmt.Errorf("lidarr %s: status %d", path, resp.StatusCode)
	}
	var raw []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]core.LidarrProfile, 0, len(raw))
	for _, r := range raw {
		out = append(out, core.LidarrProfile{ID: r.ID, Name: r.Name})
	}
	return out, nil
}

// QualityProfiles lists Lidarr's configured quality profiles (issue #331).
func (c *Client) QualityProfiles(ctx context.Context) ([]core.LidarrProfile, error) {
	return c.fetchProfiles(ctx, "/api/v1/qualityprofile")
}

// MetadataProfiles lists Lidarr's configured metadata profiles (issue #331).
func (c *Client) MetadataProfiles(ctx context.Context) ([]core.LidarrProfile, error) {
	return c.fetchProfiles(ctx, "/api/v1/metadataprofile")
}

// ErrAddArtistUncertain is returned by AddArtist when the create request
// failed at the transport level (timeout or network error) and a follow-up
// ArtistByMBID re-check could not establish whether the write actually
// landed. Verified against Lidarr 3.1.0.4875 (.lidarr-endpoints-verified.md):
// a POST /artist that exceeded the client's timeout still created the artist
// server-side, so a transport error alone is not proof of failure - it is
// only when the re-check itself also fails that the outcome is genuinely
// unknown. Callers must not treat this the same as a definite failure; see
// AddArtist's doc comment.
//
// This is an alias of core.ErrLidarrAddArtistUncertain, not a distinct
// value, so internal/app can detect it via errors.Is without importing this
// package - see that value's doc comment.
var ErrAddArtistUncertain = core.ErrLidarrAddArtistUncertain

// addArtistRecheckBudget bounds the total time recheckArtistCreated may
// spend across every attempt - generous relative to
// addArtistRecheckAttempts/addArtistRecheckInterval's defaults (3 attempts,
// 2s apart, ~4s), so it should only ever bind if a caller has widened those.
const addArtistRecheckBudget = 15 * time.Second

// recheckArtistCreated retries ArtistByMBID up to c.addArtistRecheckAttempts
// times, c.addArtistRecheckInterval apart, used by AddArtist's transport-
// error recovery path. found is false with a nil error only when every
// attempt cleanly reported the artist absent - that is the one condition
// under which AddArtist may conclude the create genuinely failed. Any
// attempt that errors at the transport level leaves the outcome unknown
// unless a later attempt finds the artist.
func (c *Client) recheckArtistCreated(ctx context.Context, foreignArtistID string) (core.LidarrArtist, bool, error) {
	var lastErr error
	for attempt := 0; attempt < c.addArtistRecheckAttempts; attempt++ {
		artist, found, err := c.ArtistByMBID(ctx, foreignArtistID)
		switch {
		case err != nil:
			lastErr = err
		case found:
			return artist, true, nil
		default:
			lastErr = nil // this attempt cleanly saw "absent"
		}
		if attempt < c.addArtistRecheckAttempts-1 {
			select {
			case <-ctx.Done():
				return core.LidarrArtist{}, false, ctx.Err()
			case <-time.After(c.addArtistRecheckInterval):
			}
		}
	}
	return core.LidarrArtist{}, false, lastErr
}

// AddArtist creates a new artist in Lidarr's library (issue #331). It always
// sends monitorNewItems:"none" and addOptions:{monitor:"none",
// searchForMissingAlbums:false}, regardless of what the caller ultimately
// wants monitored, and expects HTTP 201 with the created artist (including
// its new Lidarr id) in return.
//
// Verified by hand against Lidarr 3.1.0.4875
// (.lidarr-endpoints-verified.md): addOptions.monitor is inert on this
// version - "none", "all" and "latest" all produced 0 monitored albums, and
// sending "monitored": true at create time does not survive either.
//
// That inertness no longer matters, because this request body is now the
// whole monitoring story: app.LidarrLibrary adds the artist and then
// monitors nothing at all. A monitored album with no files lands in Lidarr's
// wanted/missing list, which pipeline.WantedSync polls, and it then creates
// a second job racing the manual download for the same album into the same
// folder - measured three seconds apart in the PR lab. Do not "fix" this by
// applying monitoring after the create; see internal/app/lidarr_library.go's
// package doc comment.
//
// On a transport error (including a timeout on addArtistHTTP), the create
// may have landed anyway - see addArtistHTTP's doc comment - so AddArtist
// re-checks ArtistByMBID before reporting failure: if the artist is now
// present, that counts as success. The re-check runs its own bounded retry
// (addArtistRecheckAttempts/addArtistRecheckInterval) rather than a single
// shot, since an immediate empty result cannot distinguish "not created"
// from "not created *yet*" - and it runs against
// context.WithoutCancel(ctx) with its own short deadline, not ctx itself:
// in production ctx is the inbound request's context, which is already
// cancelled the moment the browser gives up waiting - exactly when this
// recovery path is needed most. Only when every retry cleanly reports the
// artist absent is the original error returned as a definite failure; if
// any retry fails at the transport level and none finds the artist, the
// outcome is unknown and the error wraps ErrAddArtistUncertain.
func (c *Client) AddArtist(ctx context.Context, req core.AddArtistRequest) (core.LidarrArtist, error) {
	body := map[string]any{
		"foreignArtistId":   req.ForeignArtistID,
		"artistName":        req.ArtistName,
		"qualityProfileId":  req.QualityProfileID,
		"metadataProfileId": req.MetadataProfileID,
		"rootFolderPath":    req.RootFolderPath,
		"monitorNewItems":   "none",
		"addOptions": map[string]any{
			"monitor":                "none",
			"searchForMissingAlbums": false,
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return core.LidarrArtist{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/artist", bytes.NewReader(b))
	if err != nil {
		return core.LidarrArtist{}, err
	}
	httpReq.Header.Set("X-Api-Key", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.addArtistHTTP.Do(httpReq)
	if err != nil {
		// ctx may already be cancelled here (a request context, cancelled
		// the moment the caller gave up) - WithoutCancel plus a fresh,
		// modest deadline keeps the recovery check alive regardless.
		recheckCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), addArtistRecheckBudget)
		defer cancel()
		artist, found, checkErr := c.recheckArtistCreated(recheckCtx, req.ForeignArtistID)
		if checkErr != nil {
			return core.LidarrArtist{}, fmt.Errorf("%w: create request failed (%v) and the re-check failed too: %v", ErrAddArtistUncertain, err, checkErr)
		}
		if found {
			return artist, nil
		}
		return core.LidarrArtist{}, fmt.Errorf("lidarr add artist: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return core.LidarrArtist{}, fmt.Errorf("lidarr add artist: status %d", resp.StatusCode)
	}
	var raw struct {
		ID              int64  `json:"id"`
		ForeignArtistID string `json:"foreignArtistId"`
		ArtistName      string `json:"artistName"`
		Monitored       bool   `json:"monitored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return core.LidarrArtist{}, err
	}
	return core.LidarrArtist{ID: raw.ID, ForeignArtistID: raw.ForeignArtistID, Name: raw.ArtistName, Monitored: raw.Monitored}, nil
}

// SetArtistMonitored flips an artist's monitored flag by round-tripping the
// full artist resource - GET /api/v1/artist/{id}, mutate only "monitored" in
// place, PUT the whole body back (issue #331). Hand-building a partial PUT
// body risks silently dropping fields Lidarr expects to see on update;
// round-tripping avoids needing to know its full shape. Expects HTTP 202,
// verified against Lidarr 3.1.0.4875 (.lidarr-endpoints-verified.md).
//
// Nothing in slusk calls this: the "add to Lidarr" flow monitors nothing
// (see internal/app/lidarr_library.go's package doc comment). It stays
// because internal/lidarr is a client library - removing a working wire
// method is a separate decision from removing its one caller.
func (c *Client) SetArtistMonitored(ctx context.Context, artistID int64, monitored bool) error {
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/artist/%d", c.baseURL, artistID), nil)
	if err != nil {
		return err
	}
	getReq.Header.Set("X-Api-Key", c.apiKey)
	getResp, err := c.http.Do(getReq)
	if err != nil {
		return err
	}
	defer getResp.Body.Close()
	if getResp.StatusCode >= 300 {
		return fmt.Errorf("lidarr get artist: status %d", getResp.StatusCode)
	}
	var artist map[string]any
	if err := json.NewDecoder(getResp.Body).Decode(&artist); err != nil {
		return err
	}
	artist["monitored"] = monitored
	b, err := json.Marshal(artist)
	if err != nil {
		return err
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/v1/artist/%d", c.baseURL, artistID), bytes.NewReader(b))
	if err != nil {
		return err
	}
	putReq.Header.Set("X-Api-Key", c.apiKey)
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := c.http.Do(putReq)
	if err != nil {
		return err
	}
	defer putResp.Body.Close()
	if putResp.StatusCode >= 300 {
		return fmt.Errorf("lidarr set artist monitored: status %d", putResp.StatusCode)
	}
	return nil
}

// MonitorAlbums sets the monitored flag on a batch of album ids in one call
// (issue #331). Expects HTTP 202, verified against Lidarr 3.1.0.4875
// (.lidarr-endpoints-verified.md).
//
// Nothing in slusk calls this - see SetArtistMonitored's doc comment for
// why it stays anyway.
func (c *Client) MonitorAlbums(ctx context.Context, albumIDs []int64, monitored bool) error {
	body := map[string]any{"albumIds": albumIDs, "monitored": monitored}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/api/v1/album/monitor", bytes.NewReader(b))
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
		return fmt.Errorf("lidarr monitor albums: status %d", resp.StatusCode)
	}
	return nil
}

// AlbumsByArtist lists every album Lidarr knows about for an artist (issue
// #331), used to resolve the full set of album ids when the user chooses to
// monitor an artist's whole discography rather than just the one album that
// prompted the add.
func (c *Client) AlbumsByArtist(ctx context.Context, artistID int64) ([]core.LidarrAlbum, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/album?artistId=%d", c.baseURL, artistID), nil)
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
		return nil, fmt.Errorf("lidarr albums by artist: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID        int64 `json:"id"`
		ArtistID  int64 `json:"artistId"`
		Monitored bool  `json:"monitored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]core.LidarrAlbum, 0, len(raw))
	for _, a := range raw {
		out = append(out, core.LidarrAlbum{ID: a.ID, ArtistID: a.ArtistID, Monitored: a.Monitored})
	}
	return out, nil
}

// RunningCommands lists Lidarr's queued and in-flight commands (issue #331) -
// the asynchronous RefreshArtist run that follows AddArtist shows up here, as
// testenv/seed_lidarr.py's wait_for_idle relies on.
//
// Nothing in slusk calls this: the add flow no longer waits out that
// refresh, because it no longer applies monitoring the refresh could revert.
// See SetArtistMonitored's doc comment for why it stays anyway.
func (c *Client) RunningCommands(ctx context.Context) ([]core.LidarrCommand, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/command", nil)
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
		return nil, fmt.Errorf("lidarr command: status %d", resp.StatusCode)
	}
	var raw []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Body   struct {
			ArtistIDs []int64 `json:"artistIds"`
		} `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]core.LidarrCommand, 0, len(raw))
	for _, r := range raw {
		out = append(out, core.LidarrCommand{Name: r.Name, Status: r.Status, ArtistIDs: r.Body.ArtistIDs})
	}
	return out, nil
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
