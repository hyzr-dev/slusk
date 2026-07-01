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
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a slskd client for the given base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
}

// Result is one search result file offered by a peer.
type Result struct {
	Username string `json:"username"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	BitRate  int    `json:"bitRate"`
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
