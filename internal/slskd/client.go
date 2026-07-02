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
}

// New constructs a slskd client for the given base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:        baseURL,
		apiKey:         apiKey,
		http:           &http.Client{Timeout: 30 * time.Second},
		pollInterval:   time.Second,
		enqueueRetries: 3,
		enqueueBackoff: 500 * time.Millisecond,
		searchRetries:  2,
		searchBackoff:  time.Second,
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

// Result is one search result file offered by a peer, enriched with the peer's
// upload-availability signals (copied from the per-user response group).
type Result struct {
	Username          string `json:"username"`
	Filename          string `json:"filename"`
	Size              int64  `json:"size"`
	BitRate           int    `json:"bitRate"`
	IsLocked          bool   `json:"isLocked"`
	HasFreeUploadSlot bool   `json:"-"`
	QueueLength       int    `json:"-"`
	UploadSpeed       int    `json:"-"`
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

// Cancel cancels and removes a download by user and id.
func (c *Client) Cancel(ctx context.Context, username, id string) error {
	path := fmt.Sprintf("/api/v0/transfers/downloads/%s/%s", url.PathEscape(username), url.PathEscape(id))
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
	Files             []Result `json:"files"`
}

// Search runs a slskd search, retrying when it comes back empty. slskd's search
// database intermittently drops every response — an internal concurrency error
// that surfaces as zero results, not an HTTP error, so the same query succeeds
// moments later. Up to searchRetries extra attempts are made on an empty result
// before concluding the query genuinely has no matches. A non-empty result or a
// real error returns immediately; the caller's context bounds the total time.
func (c *Client) Search(ctx context.Context, query string, timeout time.Duration) ([]Result, error) {
	var out []Result
	var err error
	for attempt := 0; ; attempt++ {
		out, err = c.searchOnce(ctx, query, timeout)
		if err != nil || len(out) > 0 || attempt >= c.searchRetries || ctx.Err() != nil {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(c.searchBackoff):
		}
	}
}

// searchOnce starts one async slskd search, polls until it completes or timeout,
// then returns the peers' result files (locked files skipped), each enriched with
// its peer's upload-availability signals. The search is deleted from slskd after.
func (c *Client) searchOnce(ctx context.Context, query string, timeout time.Duration) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var started searchState
	if err := c.do(ctx, http.MethodPost, "/api/v0/searches", map[string]string{"searchText": query}, &started); err != nil {
		return nil, err
	}
	if started.ID == "" {
		return nil, fmt.Errorf("slskd search returned no id")
	}
	defer func() {
		// Best-effort cleanup with a fresh short context (ctx may be done).
		delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer delCancel()
		_ = c.do(delCtx, http.MethodDelete, "/api/v0/searches/"+url.PathEscape(started.ID), nil, nil)
	}()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		var st searchState
		if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(started.ID), nil, &st); err != nil {
			// If the timeout/cancel fired during the poll GET, return whatever
			// responses exist rather than a bare context error.
			if ctx.Err() != nil {
				return c.searchResponses(context.Background(), started.ID)
			}
			return nil, err
		}
		if st.IsComplete {
			break
		}
		select {
		case <-ctx.Done():
			// Timed out: return whatever responses exist so far rather than error.
			return c.searchResponses(context.Background(), started.ID)
		case <-ticker.C:
		}
	}
	return c.searchResponses(ctx, started.ID)
}

// searchResponses fetches and flattens a completed search's responses.
func (c *Client) searchResponses(ctx context.Context, id string) ([]Result, error) {
	var groups []searchResponse
	if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(id)+"/responses", nil, &groups); err != nil {
		return nil, err
	}
	var out []Result
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
