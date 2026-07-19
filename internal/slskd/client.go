// Package slskd is a thin REST client for slskd. It mirrors slskd's API and
// returns its own types; it knows nothing about slskdarr's database or job
// states. The engine defines the narrow interface it consumes from this client.
package slskd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

const (
	defaultHTTPTimeout    = 30 * time.Second
	defaultPollInterval   = time.Second
	defaultEnqueueRetries = 3
	defaultEnqueueBackoff = 500 * time.Millisecond
	defaultSearchRetries  = 2
	defaultSearchBackoff  = time.Second
	// Search cleanup must finish before the pipeline runner and runtime's
	// 10-second shutdown budgets so routine harvesting cannot race store close.
	defaultStopGrace = 9 * time.Second
)

// Client talks to a slskd instance.
type Client struct {
	baseURL        string
	apiKey         string
	http           *http.Client
	pollInterval   time.Duration
	enqueueRetries int           // extra Enqueue attempts after the first on transient failure
	enqueueBackoff time.Duration // initial delay between Enqueue retries (doubles each time)
	searchRetries  int           // extra Search attempts when a search returns zero results
	searchBackoff  time.Duration // delay between empty-result search retries
	stopGrace      time.Duration // one total deadline for stopping, finalizing, harvesting, and deleting a timed-out search
}

// New constructs a slskd client for the given base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:        baseURL,
		apiKey:         apiKey,
		http:           &http.Client{Timeout: defaultHTTPTimeout},
		pollInterval:   defaultPollInterval,
		enqueueRetries: defaultEnqueueRetries,
		enqueueBackoff: defaultEnqueueBackoff,
		searchRetries:  defaultSearchRetries,
		searchBackoff:  defaultSearchBackoff,
		stopGrace:      defaultStopGrace,
	}
}

// apiError is a non-2xx HTTP response from slskd. It carries the status code so
// callers can distinguish retryable server errors (5xx) from permanent client
// errors (4xx).
type apiError struct {
	method, path string
	status       int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("slskd %s %s: status %d", e.method, e.path, e.status)
}

// isRetryable reports whether an error from do is worth retrying: transport
// errors (timeouts, refused connections) and slskd 5xx responses are transient;
// a 4xx is our own bad request and will never succeed on retry.
func isRetryable(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status >= 500
	}
	return true
}

// IsNotFound reports whether err is a 404 response from slskd, e.g. from
// DeleteDownloadFolder when the folder never existed (a failed attempt whose
// transfers never wrote any bytes to disk) - a routine outcome for a
// best-effort cleanup, not a real failure.
func IsNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusNotFound
}

// result is one search result file offered by a peer, enriched with the
// peer's upload-availability signals (copied from the per-user response
// group) — slskd's wire shape. Search maps it to core.SearchResult so callers
// never depend on this type.
type result struct {
	Username          string `json:"username"`
	Filename          string `json:"filename"`
	Size              int64  `json:"size"`
	BitRate           int    `json:"bitRate"`
	IsLocked          bool   `json:"isLocked"`
	HasFreeUploadSlot bool   `json:"-"`
	QueueLength       int    `json:"-"`
	UploadSpeed       int    `json:"-"`
}

// toCore maps slskd's wire result shape to the provider-neutral core type.
func (r result) toCore() core.SearchResult {
	return core.SearchResult{
		Username:          r.Username,
		Filename:          r.Filename,
		Size:              r.Size,
		BitRate:           r.BitRate,
		HasFreeUploadSlot: r.HasFreeUploadSlot,
		QueueLength:       r.QueueLength,
		UploadSpeed:       r.UploadSpeed,
	}
}

// Transfer is one download slskd currently knows about.
type Transfer struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Filename         string `json:"filename"`
	State            string `json:"state"`
	Size             int64  `json:"size"`
	BytesTransferred int64  `json:"bytesTransferred"`
	// Exception carries slskd's failure reason for a terminal transfer, e.g. the
	// peer's "Too many megabytes" rejection. Used to decide whether a failure is
	// worth retrying (transient) or terminal (e.g. "File not shared").
	Exception string `json:"exception"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &apiError{method: method, path: path, status: resp.StatusCode}
	}
	if out != nil {
		// Tolerate an empty body (e.g. 201 Created with no content): EOF is not
		// an error here. Enqueue relies on this.
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

// Enqueue starts a download of one file from a user and returns slskd's id. The
// size is REQUIRED — slskd requests the file from the peer by (filename, size),
// and a zero size makes every transfer fail immediately (TimedOut/Cancelled).
// The id may be empty if slskd's response carries no body; the reconciler then
// backfills it via the (username, filename) fallback key.
//
// Transient failures (request timeout, slskd 5xx) are retried with exponential
// backoff so one flaky POST does not doom an otherwise-good candidate — slskd
// stalls enqueues under load, and abandoning the whole release for a single
// stall is wasteful. Retry can duplicate a request whose response was lost after
// slskd already accepted it, but slskd keys downloads by (username, filename) so
// a duplicate resolves to the same transfer; the reconciler's fallback key does
// the same on our side. The caller's context bounds the total wait.
func (c *Client) Enqueue(ctx context.Context, username, filename string, size int64) (string, error) {
	body := []map[string]any{{"filename": filename, "size": size}}
	path := "/api/v0/transfers/downloads/" + url.PathEscape(username)
	backoff := c.enqueueBackoff
	var resp struct {
		ID string `json:"id"`
	}
	var err error
	for attempt := 0; ; attempt++ {
		resp.ID = ""
		err = c.do(ctx, http.MethodPost, path, body, &resp)
		if err == nil || attempt >= c.enqueueRetries || !isRetryable(err) {
			return resp.ID, err
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// downloadsResponse mirrors slskd's grouped-by-user download listing.
type downloadsResponse []struct {
	Username    string `json:"username"`
	Directories []struct {
		Files []Transfer `json:"files"`
	} `json:"directories"`
}

// ListDownloads returns every download slskd currently tracks, flattened.
func (c *Client) ListDownloads(ctx context.Context) ([]Transfer, error) {
	var grouped downloadsResponse
	if err := c.do(ctx, http.MethodGet, "/api/v0/transfers/downloads", nil, &grouped); err != nil {
		return nil, err
	}
	var out []Transfer
	for _, u := range grouped {
		for _, d := range u.Directories {
			for _, f := range d.Files {
				f.Username = u.Username
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// Cancel cancels a still-active slskd download by user and id (DELETE without
// ?remove=true). It stops an in-flight transfer but leaves the resulting
// terminal record behind; use Remove to purge that record once a transfer has
// reached a terminal state.
func (c *Client) Cancel(ctx context.Context, username, id string) error {
	path := fmt.Sprintf("/api/v0/transfers/downloads/%s/%s", url.PathEscape(username), url.PathEscape(id))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// Remove purges a slskd download record entirely (DELETE with ?remove=true).
// Unlike Cancel — which only cancels a still-active transfer and leaves the
// terminal record behind — Remove is for a transfer that has already reached a
// terminal state: it deletes the leftover record so slskd's transfer list does
// not accumulate every download slskdarr has ever made.
func (c *Client) Remove(ctx context.Context, username, id string) error {
	path := fmt.Sprintf("/api/v0/transfers/downloads/%s/%s?remove=true", url.PathEscape(username), url.PathEscape(id))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteDownloadFolder deletes a subdirectory (and its contents) from slskd's
// downloads root, addressed by its name relative to that root (no path
// separators). Used to purge a failed candidate attempt's leftover files
// before the next attempt starts, so they don't get mixed into the next
// attempt's local folder (slskd names local subfolders after the remote
// peer's own leaf directory name, so two different peers sharing an
// identically-named folder can otherwise collide).
func (c *Client) DeleteDownloadFolder(ctx context.Context, name string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(name))
	path := fmt.Sprintf("/api/v0/files/downloads/directories/%s", encoded)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// searchState is the subset of a slskd search object used for completion polling.
type searchState struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	IsComplete bool   `json:"isComplete"`
}

// searchResponse is one peer's grouped response to a search.
type searchResponse struct {
	Username          string   `json:"username"`
	HasFreeUploadSlot bool     `json:"hasFreeUploadSlot"`
	QueueLength       int      `json:"queueLength"`
	UploadSpeed       int      `json:"uploadSpeed"`
	Files             []result `json:"files"`
}

// Search runs a slskd search, retrying when it comes back empty. slskd's search
// database intermittently drops every response — an internal concurrency error
// that surfaces as zero results, not an HTTP error, so the same query succeeds
// moments later. Up to searchRetries extra attempts are made on an empty result
// before concluding the query genuinely has no matches. A non-empty result or a
// real error returns immediately; the caller's context bounds the total time.
func (c *Client) Search(ctx context.Context, query string, timeout time.Duration) ([]core.SearchResult, error) {
	var out []result
	var err error
	for attempt := 0; ; attempt++ {
		out, err = c.searchOnce(ctx, query, timeout)
		if err != nil || len(out) > 0 || attempt >= c.searchRetries || ctx.Err() != nil {
			return toCoreResults(out), err
		}
		select {
		case <-ctx.Done():
			return toCoreResults(out), err
		case <-time.After(c.searchBackoff):
		}
	}
}

// toCoreResults maps a slice of slskd's wire result shape to the
// provider-neutral core type.
func toCoreResults(in []result) []core.SearchResult {
	if in == nil {
		return nil
	}
	out := make([]core.SearchResult, len(in))
	for i, r := range in {
		out[i] = r.toCore()
	}
	return out
}

// searchOnce starts one async slskd search, polls until it completes or timeout,
// then returns the peers' result files (locked files skipped), each enriched with
// its peer's upload-availability signals. The search is deleted from slskd once
// its responses are harvested — but ONLY after the search has finalized
// (isComplete): deleting it any earlier used to race slskd's own async finalize
// of the same search, which surfaced as EF Core "affected 0 rows" errors and
// could drop responses. On the timeout path below, completion (and the delete)
// is handled by stopAndHarvest, which deletes only once slskd reports the
// search complete and otherwise leaves it for slskd's own retention.
func (c *Client) searchOnce(ctx context.Context, query string, timeout time.Duration) ([]result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var started searchState
	if err := c.do(ctx, http.MethodPost, "/api/v0/searches", map[string]string{"searchText": query}, &started); err != nil {
		return nil, err
	}
	if started.ID == "" {
		return nil, fmt.Errorf("slskd search returned no id")
	}

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		var st searchState
		if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(started.ID), nil, &st); err != nil {
			// If the timeout/cancel fired during the poll GET, stop the search
			// and harvest what it found rather than return a bare context error.
			if ctx.Err() != nil {
				return c.stopAndHarvest(ctx, started.ID)
			}
			return nil, err
		}
		if st.IsComplete {
			break
		}
		select {
		case <-ctx.Done():
			// Timed out: stop the search and harvest what it found so far.
			return c.stopAndHarvest(ctx, started.ID)
		case <-ticker.C:
		}
	}
	res, err := c.searchResponses(ctx, started.ID)
	if err == nil {
		// The poll loop broke on isComplete, so slskd finalized this search and
		// its responses are now in hand: safe to remove it. On a harvest error we
		// leave the search for slskd's own retention to reap rather than deleting
		// responses we never managed to read.
		c.deleteSearch(ctx, started.ID)
	}
	return res, err
}

// stopAndHarvest asks slskd to stop a still-in-progress search, waits for it to
// finalize, then harvests its responses. It exists because slskd only persists
// a search's responses when the search is finalized: GET /responses on an
// InProgress search returns an empty list even when the search state already
// reports a large responseCount (verified live: at t=20s a search was
// InProgress with responseCount=42 while /responses returned 0 groups; the
// moment it completed, everything appeared). Harvesting at our own timeout
// without stopping therefore ALWAYS yielded zero results for any search slower
// than search_timeout. The wait-for-isComplete after the stop is deliberate:
// harvesting (or deleting — see searchOnce's doc comment) while slskd's own
// async finalize is still running is the same race that used to surface as EF
// Core "affected 0 rows" errors. If the search never finalizes within
// stopGrace, a best-effort harvest happens anyway — and in that fallback case
// the search is deliberately left undeleted for the same reason: isComplete
// was never observed, so we cannot be sure slskd has finished writing it.
func (c *Client) stopAndHarvest(parent context.Context, id string) ([]result, error) {
	// The caller may already be cancelled. Every cleanup request shares this one
	// independent deadline; no fallback stage may restart the grace period.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(parent), c.stopGrace)
	defer cancelCleanup()

	// Reserve part of the total budget for one last response harvest when slskd
	// never reports completion. This is only an internal phase deadline; the
	// cleanupCtx deadline remains the single upper bound for all cleanup work.
	reserve := c.stopGrace / 2
	if reserve > time.Second {
		reserve = time.Second
	}
	cleanupDeadline, _ := cleanupCtx.Deadline()
	finalizeCtx, cancelFinalize := context.WithDeadline(cleanupCtx, cleanupDeadline.Add(-reserve))
	defer cancelFinalize()

	// Best-effort: the search may have completed on its own in the meantime,
	// in which case slskd has nothing to cancel and may answer with an error.
	_ = c.do(cleanupCtx, http.MethodPut, "/api/v0/searches/"+url.PathEscape(id), nil, nil)

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		var st searchState
		if err := c.do(finalizeCtx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(id), nil, &st); err == nil && st.IsComplete {
			res, err := c.searchResponses(cleanupCtx, id)
			if err == nil {
				// Finalized (isComplete) before harvest, so deleting is safe once
				// the responses are in hand; on a harvest error we leave it for
				// slskd's retention rather than dropping unread responses.
				c.deleteSearch(cleanupCtx, id)
			}
			return res, err
		}
		select {
		case <-finalizeCtx.Done():
			// Completion was not observed in the finalize phase. Use only the
			// time remaining on the same total deadline for a final harvest and
			// do not delete a search that may still be finalizing.
			return c.searchResponses(cleanupCtx, id)
		case <-ticker.C:
		}
	}
}

// searchResponses fetches and flattens a completed search's responses.
func (c *Client) searchResponses(ctx context.Context, id string) ([]result, error) {
	var groups []searchResponse
	if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(id)+"/responses", nil, &groups); err != nil {
		return nil, err
	}
	var out []result
	for _, g := range groups {
		for _, f := range g.Files {
			if f.IsLocked {
				continue
			}
			f.Username = g.Username
			f.HasFreeUploadSlot = g.HasFreeUploadSlot
			f.QueueLength = g.QueueLength
			f.UploadSpeed = g.UploadSpeed
			out = append(out, f)
		}
	}
	return out, nil
}

// deleteSearch best-effort removes a finished search from slskd (DELETE
// /api/v0/searches/{id}). Discovery issues many searches per album; left in
// place they pile up in slskd's search history and make its UI unresponsive.
// It is called ONLY after a search's responses have been harvested following
// completion (see searchOnce and stopAndHarvest) — deleting while slskd is
// still finalizing the search is the same async-write race that used to surface
// as EF Core "affected 0 rows" and could drop responses. Errors are swallowed:
// a failed delete must never fail the search, and slskd's own retention config
// is the backstop for records that slip through (e.g. a crash between harvest
// and delete).
func (c *Client) deleteSearch(ctx context.Context, id string) {
	_ = c.do(ctx, http.MethodDelete, "/api/v0/searches/"+url.PathEscape(id), nil, nil)
}
