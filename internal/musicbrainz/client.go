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
	"strings"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	"github.com/samuelenocsson/slusk/internal/core"
)

// DefaultBaseURL is MusicBrainz's public API root.
const DefaultBaseURL = "https://musicbrainz.org"

// DefaultTimeout bounds a single HTTP request.
const DefaultTimeout = 10 * time.Second

// DefaultCacheTTL is how long a response is reused before it is fetched again.
const DefaultCacheTTL = time.Hour

// searchReleaseGroupsLimit bounds how many rows MusicBrainz returns per
// call - generous enough for a human to scan in the identify modal without
// paging, small enough to keep the response cheap.
const searchReleaseGroupsLimit = 25

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
	return fmt.Sprintf("slusk/1.0 (%s)", c.contact)
}

// get performs a rate-limited, cached GET against reqURL and decodes the JSON
// body into out. A cache hit skips both the rate limiter and the network
// call entirely - only an actual outgoing HTTP request needs to wait for a
// token, per MusicBrainz's 1 req/s policy. label identifies the call in
// error messages ("release-group search", "releases") - it must never carry
// user input, unlike reqURL, which embeds the free-text query.
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

// luceneSpecial is every Lucene metacharacter MusicBrainz's query parser
// treats specially: + - && || ! ( ) { } [ ] ^ " ~ * ? : \ /. Two of them,
// && and ||, are two-character operators, but escaping each of their
// constituent runes individually (every & and every |) is sufficient to
// defuse them - a lone "\&" can never combine with a following "\&" to form
// the "&&" operator.
const luceneSpecial = `+-&|!(){}[]^"~*?:\/`

// isLuceneKeyword reports whether token is one of Lucene's boolean/range
// operators when it stands alone. The keywords are case-sensitive in
// Lucene's own grammar - a lowercase "not" is never an operator - so this
// check must be exact-case, not case-insensitive.
func isLuceneKeyword(token string) bool {
	switch token {
	case "AND", "OR", "NOT", "TO":
		return true
	}
	return false
}

// escapeLucene backslash-escapes every Lucene metacharacter in s so it is
// interpolated into a MusicBrainz query as a literal, never as an operator.
// This matters because unescaped input is not a hard error - MusicBrainz's
// parser is tolerant of stray operators - which is exactly why skipping this
// is easy to miss: a folder name's brackets and parentheses silently turn a
// targeted search into a query matching over a million release-groups that
// still happens to rank correctly for the easy cases (issue #321).
//
// It also neutralises AND/OR/NOT/TO when a whitespace-delimited token is
// exactly one of them: inside releasegroup:(...) those are parsed as boolean
// operators, not literal words, so an album title like Garbage's "Not Your
// Kind of People" loses its leading NOT to negation and the correct
// release-group cannot be found at all (measured against the live API -
// issue #321). Escaping the token's first rune is enough to stop the parser
// recognising it as a keyword. A word that merely contains a keyword, such
// as NOTHING or ANDROMEDA, is left untouched - only an exact, whole-token
// match is neutralised.
func escapeLucene(s string) string {
	var b strings.Builder
	var token []rune
	flush := func() {
		if len(token) == 0 {
			return
		}
		if isLuceneKeyword(string(token)) {
			b.WriteByte('\\')
		}
		for _, r := range token {
			if strings.ContainsRune(luceneSpecial, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		token = token[:0]
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			flush()
			b.WriteRune(r)
			continue
		}
		token = append(token, r)
	}
	flush()
	return b.String()
}

// wireReleaseGroupQuery is GET /ws/2/release-group?query=...'s response
// shape - MusicBrainz's Lucene search, distinct from the browse-by-artist
// endpoint this replaces. Note the two "count" fields at different levels:
// the top-level one is the search's total hit count, the per-hit one is that
// release-group's own edition count (verified against the live API - issue
// #321 - as exactly the number of releases internal/musicbrainz.Client's
// Releases method would return for the same id).
type wireReleaseGroupQuery struct {
	Count         int `json:"count"`
	ReleaseGroups []struct {
		ID               string   `json:"id"`
		Title            string   `json:"title"`
		Score            int      `json:"score"`
		Count            int      `json:"count"`
		FirstReleaseDate string   `json:"first-release-date"`
		PrimaryType      string   `json:"primary-type"`
		SecondaryTypes   []string `json:"secondary-types"`
		ArtistCredit     []struct {
			Name   string `json:"name"`
			Artist struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"artist-credit"`
	} `json:"release-groups"`
}

// releaseGroupSearchQuery builds the Lucene query string for
// SearchReleaseGroups: artist:(<artist>) AND releasegroup:(<album>), or just
// releasegroup:(<album>) when artist is blank. Both terms are escaped with
// escapeLucene before interpolation; fuzzy appends "~" inside the album
// term's parens, MusicBrainz's own syntax for a fuzzy-matched term.
func releaseGroupSearchQuery(artist, album string, fuzzy bool) string {
	albumTerm := escapeLucene(album)
	if fuzzy {
		albumTerm += "~"
	}
	query := fmt.Sprintf("releasegroup:(%s)", albumTerm)
	if artist != "" {
		query = fmt.Sprintf("artist:(%s) AND %s", escapeLucene(artist), query)
	}
	return query
}

// SearchReleaseGroups runs a single combined artist+album search against
// MusicBrainz's release-group search endpoint (issue #321), replacing the
// old two-step SearchArtists+ReleaseGroups flow: that flow could not supply
// an edition count, and the obvious alternative - a release-group search
// filtered only by arid: - comes back with every hit at score=100 in an
// order that buries the canonical albums (Metallica's first 100 such
// results contain neither "Ride the Lightning" nor "Master of Puppets").
//
// artist may be blank - the query then omits the artist:() clause entirely
// - but an album-only search ranks poorly: MusicBrainz's own relevance
// scoring does not reliably surface the right release-group without an
// artist to disambiguate, so callers should not treat a blank artist as
// equivalent to a targeted search, only as a degraded one.
//
// album must not be blank: a blank album term builds releasegroup:(), which
// MusicBrainz's Lucene parser rejects with a 400. Client does not guard
// against this itself - internal/app.Identify.SearchReleaseGroups is the
// only caller and already rejects a blank album with
// ErrIdentifyQueryInvalid before reaching here.
//
// When the strict query returns zero hits, SearchReleaseGroups retries once
// with a fuzzy album term (releasegroup:(<album>~)) - the fuzzy retry only
// fires on a miss, never as the default. "~" binds only to the last
// whitespace-delimited term in the album, so this rescues a typo in that
// term (e.g. "lightening" in "ride the lightening") but not one earlier in
// the title; it is not a general typo-tolerant search. If the retry itself
// fails, the strict leg's empty result is returned instead of the retry's
// error: a legitimate zero-hit search is a normal outcome, not a failure, and
// it must not be turned into ErrIdentifyUnavailable just because the
// best-effort retry for it happened to error. The returned slice is capped
// at searchReleaseGroupsLimit; total is MusicBrainz's reported match count
// from whichever request produced the returned slice, which the caller must
// compare against len(slice) to detect truncation - it is never inferred
// from the slice alone.
func (c *Client) SearchReleaseGroups(ctx context.Context, artist, album string) ([]core.MBReleaseGroup, int, error) {
	groups, total, err := c.searchReleaseGroups(ctx, artist, album, false)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		fuzzyGroups, fuzzyTotal, fuzzyErr := c.searchReleaseGroups(ctx, artist, album, true)
		if fuzzyErr != nil {
			return groups, total, nil
		}
		return fuzzyGroups, fuzzyTotal, nil
	}
	return groups, total, nil
}

func (c *Client) searchReleaseGroups(ctx context.Context, artist, album string, fuzzy bool) ([]core.MBReleaseGroup, int, error) {
	query := releaseGroupSearchQuery(artist, album, fuzzy)
	reqURL := fmt.Sprintf("%s/ws/2/release-group?query=%s&fmt=json&limit=%d",
		c.baseURL, url.QueryEscape(query), searchReleaseGroupsLimit)
	var body wireReleaseGroupQuery
	if err := c.get(ctx, reqURL, "release-group search", &body); err != nil {
		return nil, 0, err
	}
	out := make([]core.MBReleaseGroup, 0, len(body.ReleaseGroups))
	for _, rg := range body.ReleaseGroups {
		var artistName, artistID string
		if len(rg.ArtistCredit) > 0 {
			artistName, artistID = rg.ArtistCredit[0].Name, rg.ArtistCredit[0].Artist.ID
		}
		out = append(out, core.MBReleaseGroup{
			ID: rg.ID, Title: rg.Title, ArtistName: artistName, ArtistID: artistID,
			FirstReleaseDate: rg.FirstReleaseDate, PrimaryType: rg.PrimaryType,
			SecondaryTypes: rg.SecondaryTypes, EditionCount: rg.Count, Score: rg.Score,
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
