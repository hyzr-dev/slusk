// Package slskd is a thin REST client for slskd. It mirrors slskd's API and
// returns its own types; it knows nothing about slskdarr's database or job
// states. The engine defines the narrow interface it consumes from this client.
package slskd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client talks to a slskd instance.
type Client struct {
	baseURL      string
	apiKey       string
	http         *http.Client
	pollInterval time.Duration
}

// New constructs a slskd client for the given base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}, pollInterval: time.Second}
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
		return fmt.Errorf("slskd %s %s: status %d", method, path, resp.StatusCode)
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
// id may be empty if slskd's response carries no body; the reconciler then
// backfills it via the (username, filename) fallback key.
func (c *Client) Enqueue(ctx context.Context, username, filename string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	body := []map[string]string{{"filename": filename}}
	err := c.do(ctx, http.MethodPost, "/api/v0/transfers/downloads/"+url.PathEscape(username), body, &resp)
	return resp.ID, err
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

// Search starts an async slskd search, polls until it completes or timeout, then
// returns the peers' result files (locked files skipped), each enriched with its
// peer's upload-availability signals. The search is deleted from slskd afterward.
func (c *Client) Search(ctx context.Context, query string, timeout time.Duration) ([]Result, error) {
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
