// Package musicbrainz is a thin REST client for the MusicBrainz web service
// (issue #321). It follows internal/lidarr and internal/slskd's shape:
// unexported wire structs matching the provider's JSON, a toCore mapping per
// type, and New returning a plain struct with no interface in this package -
// consumers (internal/app) declare their own narrow interface.
//
// MusicBrainz allows one request per second and requires every client to
// identify itself with a contact-bearing User-Agent; violating either gets
// the whole app's IP blocked, which would break the feature for every user,
// not just whoever triggered the offending request. Client enforces both:
// every outgoing request waits on a 1 req/s rate limiter, and a Client built
// without a contact refuses to make requests at all rather than send an
// anonymous User-Agent.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// DefaultBaseURL is MusicBrainz's public API root.
const DefaultBaseURL = "https://musicbrainz.org"

// DefaultTimeout bounds a single HTTP request.
const DefaultTimeout = 10 * time.Second

// DefaultCacheTTL is how long a response is reused before it is fetched again.
const DefaultCacheTTL = time.Hour

// searchArtistsLimit and releaseGroupsLimit bound how many rows MusicBrainz
// returns per call - generous enough for a human to scan in the identify
// modal without paging, small enough to keep the response cheap.
const searchArtistsLimit = 25
const releaseGroupsLimit = 100

// releasesLimit is fixed at MusicBrainz's page-size ceiling: Releases fetches
// every edition of a release-group in one call, and 100 releases for a single
// release-group is already far beyond anything real (issue #321's worked
// example, Metallica's "Ride the Lightning", has 60).
const releasesLimit = 100

// ErrNoContact is returned by every request method when the client was
// constructed without a contact: MusicBrainz requires a User-Agent that
// identifies the application with contact info, and sending an unidentified
// request is what gets an IP blocked.
var ErrNoContact = errors.New("musicbrainz: client requires a contact to identify the application")

// Client talks to the MusicBrainz web service.
type Client struct {
	baseURL string
	// contact identifies the application in the User-Agent, e.g. an email
	// address or a URL, per MusicBrainz's User-Agent policy.
	contact string
	http    *http.Client
	limiter *rate.Limiter
	cache   *ttlCache
}

// Option configures a Client at construction.
type Option func(*Client)

// WithTimeout overrides the per-request HTTP timeout (default DefaultTimeout).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.http.Timeout = d
		}
	}
}

// WithCacheTTL overrides how long a response is cached (default DefaultCacheTTL).
func WithCacheTTL(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.cache.ttl = d
		}
	}
}

// New constructs a MusicBrainz client. baseURL defaults to DefaultBaseURL
// when blank. contact is required for every request to succeed - see
// ErrNoContact - but is not validated here, matching internal/lidarr and
// internal/slskd's New, which never reject their arguments either.
func New(baseURL, contact string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL: baseURL,
		contact: contact,
		http:    &http.Client{Timeout: DefaultTimeout},
		limiter: rate.NewLimiter(rate.Limit(1), 1),
		cache:   newTTLCache(DefaultCacheTTL),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// userAgent builds the identifying User-Agent MusicBrainz's usage policy
// requires: "application/version (contact)".
func (c *Client) userAgent() string {
	return fmt.Sprintf("slskdarr/1.0 (%s)", c.contact)
}

// get performs a rate-limited, cached GET against reqURL and decodes the JSON
// body into out. A cache hit skips both the rate limiter and the network
// call entirely - only an actual outgoing HTTP request needs to wait for a
// token, per MusicBrainz's 1 req/s policy. label identifies the call in
// error messages ("artist search", "release groups", "releases") - it must
// never carry user input, unlike reqURL, which embeds the free-text query.
func (c *Client) get(ctx context.Context, reqURL, label string, out any) error {
	if c.contact == "" {
		return ErrNoContact
	}
	if body, ok := c.cache.get(reqURL); ok {
		return json.Unmarshal(body, out)
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("musicbrainz %s: status %d", label, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	c.cache.set(reqURL, body)
	return nil
}

// wireArtistSearch is GET /ws/2/artist's response shape.
type wireArtistSearch struct {
	Count   int `json:"count"`
	Artists []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		Type           string `json:"type"`
		Country        string `json:"country"`
		Disambiguation string `json:"disambiguation"`
		Score          int    `json:"score"`
	} `json:"artists"`
}

// SearchArtists searches MusicBrainz artists by free-text query. The
// returned slice is capped at searchArtistsLimit; total is MusicBrainz's
// reported match count, which the caller must compare against len(slice) to
// detect truncation - it is never inferred from the slice alone.
func (c *Client) SearchArtists(ctx context.Context, query string) ([]core.MBArtist, int, error) {
	reqURL := fmt.Sprintf("%s/ws/2/artist?query=%s&fmt=json&limit=%d",
		c.baseURL, url.QueryEscape(query), searchArtistsLimit)
	var body wireArtistSearch
	if err := c.get(ctx, reqURL, "artist search", &body); err != nil {
		return nil, 0, err
	}
	out := make([]core.MBArtist, 0, len(body.Artists))
	for _, a := range body.Artists {
		out = append(out, core.MBArtist{
			ID: a.ID, Name: a.Name, Type: a.Type,
			Country: a.Country, Disambiguation: a.Disambiguation, Score: a.Score,
		})
	}
	return out, body.Count, nil
}

// wireReleaseGroupSearch is GET /ws/2/release-group's response shape.
type wireReleaseGroupSearch struct {
	Count         int `json:"release-group-count"`
	ReleaseGroups []struct {
		ID               string   `json:"id"`
		Title            string   `json:"title"`
		FirstReleaseDate string   `json:"first-release-date"`
		PrimaryType      string   `json:"primary-type"`
		SecondaryTypes   []string `json:"secondary-types"`
	} `json:"release-groups"`
}

// ReleaseGroups lists an artist's albums (release-groups of type "album").
// The returned slice is capped at releaseGroupsLimit; total is
// MusicBrainz's reported match count, which the caller must compare against
// len(slice) to detect truncation - it is never inferred from the slice
// alone.
func (c *Client) ReleaseGroups(ctx context.Context, artistMBID string) ([]core.MBReleaseGroup, int, error) {
	reqURL := fmt.Sprintf("%s/ws/2/release-group?artist=%s&type=album&fmt=json&limit=%d",
		c.baseURL, url.QueryEscape(artistMBID), releaseGroupsLimit)
	var body wireReleaseGroupSearch
	if err := c.get(ctx, reqURL, "release groups", &body); err != nil {
		return nil, 0, err
	}
	out := make([]core.MBReleaseGroup, 0, len(body.ReleaseGroups))
	for _, rg := range body.ReleaseGroups {
		out = append(out, core.MBReleaseGroup{
			ID: rg.ID, Title: rg.Title, FirstReleaseDate: rg.FirstReleaseDate,
			PrimaryType: rg.PrimaryType, SecondaryTypes: rg.SecondaryTypes,
		})
	}
	return out, body.Count, nil
}

// wireReleaseList is GET /ws/2/release?inc=media's response shape.
type wireReleaseList struct {
	Count    int `json:"release-count"`
	Releases []struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Date    string `json:"date"`
		Country string `json:"country"`
		Status  string `json:"status"`
		Media   []struct {
			Format     string `json:"format"`
			TrackCount int    `json:"track-count"`
		} `json:"media"`
	} `json:"releases"`
}

// Releases lists every edition of a release-group, each with its own track
// count (see core.MBRelease's doc comment on why this is not collapsed to a
// band here). TrackCount is the sum of every medium's track-count - a
// multi-disc edition reports several media entries. A release with no media
// data at all gets TrackCountKnown = false rather than a fabricated 0. The
// returned slice is capped at releasesLimit; total is MusicBrainz's
// reported match count, which the caller must compare against len(slice) to
// detect truncation - it is never inferred from the slice alone.
func (c *Client) Releases(ctx context.Context, releaseGroupMBID string) ([]core.MBRelease, int, error) {
	reqURL := fmt.Sprintf("%s/ws/2/release?release-group=%s&inc=media&fmt=json&limit=%d",
		c.baseURL, url.QueryEscape(releaseGroupMBID), releasesLimit)
	var body wireReleaseList
	if err := c.get(ctx, reqURL, "releases", &body); err != nil {
		return nil, 0, err
	}
	out := make([]core.MBRelease, 0, len(body.Releases))
	for _, r := range body.Releases {
		rel := core.MBRelease{ID: r.ID, Title: r.Title, Date: r.Date, Country: r.Country, Status: r.Status}
		if len(r.Media) > 0 {
			rel.TrackCountKnown = true
			for _, m := range r.Media {
				rel.TrackCount += m.TrackCount
			}
		}
		out = append(out, rel)
	}
	return out, body.Count, nil
}
