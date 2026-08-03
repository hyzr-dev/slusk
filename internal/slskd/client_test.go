package slskd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

func TestEnqueueSendsFilenameAndSize(t *testing.T) {
	var body []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			t.Errorf("missing api key header")
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]string{"id": "guid-123"})
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	id, err := c.Enqueue(context.Background(), "bob", "album/01.flac", 12345)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id != "guid-123" {
		t.Errorf("id = %q, want guid-123", id)
	}
	// slskd needs the size to request the file — a missing/zero size fails every
	// transfer. Assert both fields are sent.
	if len(body) != 1 || body[0]["filename"] != "album/01.flac" {
		t.Fatalf("filename not sent: %+v", body)
	}
	if body[0]["size"] != float64(12345) { // JSON numbers decode to float64
		t.Errorf("size not sent, got %v", body[0]["size"])
	}
}

func TestEnqueueRetriesOnServerError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Fail with a transient 503 the first two times, then succeed. slskd can
		// briefly reject/stall enqueues under load; a good candidate must not be
		// abandoned because of that.
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": "guid-ok"})
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.enqueueBackoff = time.Millisecond // keep the test fast
	id, err := c.Enqueue(context.Background(), "bob", "album/01.flac", 12345)
	if err != nil {
		t.Fatalf("Enqueue should retry through transient 503s: %v", err)
	}
	if id != "guid-ok" {
		t.Errorf("id = %q, want guid-ok", id)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts (2 failures + 1 success), got %d", calls)
	}
}

func TestEnqueueDoesNotRetryClientError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A 4xx is our fault (bad request) — retrying cannot help, so it must fail fast.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.enqueueBackoff = time.Millisecond
	_, err := c.Enqueue(context.Background(), "bob", "album/01.flac", 12345)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if calls != 1 {
		t.Errorf("4xx must not be retried; expected 1 attempt, got %d", calls)
	}
}

func TestDeleteDownloadFolderSendsBase64EncodedName(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if err := c.DeleteDownloadFolder(context.Background(), "1000 Forms of Fear (2014)"); err != nil {
		t.Fatalf("DeleteDownloadFolder: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	wantPath := "/api/v0/files/downloads/directories/" + base64.StdEncoding.EncodeToString([]byte("1000 Forms of Fear (2014)"))
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
}

func TestDeleteDownloadFolderReturnsErrorOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if err := c.DeleteDownloadFolder(context.Background(), "A"); err == nil {
		t.Fatal("expected error on non-2xx response")
	}
}

func TestNotFoundWrapsErrRemoteNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	err := c.DeleteDownloadFolder(context.Background(), "A")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrRemoteNotFound) {
		t.Errorf("errors.Is(%v, core.ErrRemoteNotFound) = false, want true", err)
	}
}

func TestOtherStatusesDoNotWrapErrRemoteNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	err := c.DeleteDownloadFolder(context.Background(), "A")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, core.ErrRemoteNotFound) {
		t.Errorf("errors.Is(%v, core.ErrRemoteNotFound) = true, want false", err)
	}
}

func TestRemoveSendsRemoveTrueQuery(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	if err := c.Remove(context.Background(), "user", "abc"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v0/transfers/downloads/user/abc" {
		t.Errorf("path = %q, want /api/v0/transfers/downloads/user/abc", gotPath)
	}
	if gotQuery != "remove=true" {
		t.Errorf("query = %q, want remove=true", gotQuery)
	}
}

func TestListDownloadsFlattens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"username":"bob","directories":[{"files":[
		    {"id":"g1","filename":"a.flac","state":"InProgress","size":100,"bytesTransferred":40}
		  ]}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	got, err := c.ListDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListDownloads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "g1" || got[0].Username != "bob" {
		t.Fatalf("unexpected transfers: %+v", got)
	}
	if got[0].BytesDone != 40 {
		t.Errorf("bytesDone = %d", got[0].BytesDone)
	}
}

// TestTransferToCore covers transfer.toCore()'s translation of slskd's wire
// state string and failure reason: State (via mapTransferState) and
// Retryable (via isTransientFailure) together, since both are private and
// only reachable through the adapter's ListDownloads mapping otherwise.
func TestTransferToCore(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		exception     string
		wantState     core.TransferState
		wantRetryable bool
	}{
		{name: "succeeded", state: "Completed, Succeeded", wantState: core.TransferCompleted, wantRetryable: true},
		{name: "cancelled", state: "Completed, Cancelled", wantState: core.TransferCancelled, wantRetryable: true},
		{name: "canceled (American spelling)", state: "Completed, Canceled", wantState: core.TransferCancelled, wantRetryable: true},
		{name: "timed out", state: "Completed, TimedOut", wantState: core.TransferErrored, wantRetryable: true},
		{name: "aborted", state: "Completed, Aborted", wantState: core.TransferErrored, wantRetryable: true},
		{
			name: "rejected, transient (too many megabytes)", state: "Completed, Rejected",
			exception: "Too many megabytes", wantState: core.TransferErrored, wantRetryable: true,
		},
		{
			name: "rejected, permanent (file not shared)", state: "Completed, Rejected",
			exception: "File not shared.", wantState: core.TransferErrored, wantRetryable: false,
		},
		{
			name: "errored, permanent (not shared)", state: "Completed, Errored",
			exception: "not shared", wantState: core.TransferErrored, wantRetryable: false,
		},
		{
			name: "errored, permanent (banned)", state: "Completed, Errored",
			exception: "banned", wantState: core.TransferErrored, wantRetryable: false,
		},
		{
			name: "errored, permanent, case-insensitive", state: "Completed, Errored",
			exception: "Banned", wantState: core.TransferErrored, wantRetryable: false,
		},
		{name: "errored, empty exception is retryable", state: "Completed, Errored", exception: "", wantState: core.TransferErrored, wantRetryable: true},
		{name: "in progress", state: "InProgress", wantState: core.TransferInProgress, wantRetryable: true},
		{name: "queued", state: "Queued", wantState: core.TransferQueued, wantRetryable: true},
		{name: "unrecognized state falls back to queued", state: "SomethingNew", wantState: core.TransferQueued, wantRetryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := transfer{
				ID: "g1", Username: "bob", Filename: "a.flac",
				State: tt.state, Size: 100, BytesTransferred: 50, Exception: tt.exception,
			}
			got := tr.toCore()
			if got.State != tt.wantState {
				t.Errorf("State = %v, want %v", got.State, tt.wantState)
			}
			if got.Retryable != tt.wantRetryable {
				t.Errorf("Retryable = %v, want %v", got.Retryable, tt.wantRetryable)
			}
			if got.Failure != tt.exception {
				t.Errorf("Failure = %q, want %q", got.Failure, tt.exception)
			}
			if got.ID != "g1" || got.Username != "bob" || got.Filename != "a.flac" || got.Size != 100 || got.BytesDone != 50 {
				t.Errorf("passthrough fields not preserved: %+v", got)
			}
		})
	}
}

func TestSearchPollsThenReturnsFlattenedResults(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			polls++
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[
			  {"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1000,
			   "files":[
			     {"filename":"@@x\\A\\01.flac","size":100,"bitRate":900,"isLocked":false},
			     {"filename":"@@x\\A\\locked.flac","size":100,"bitRate":900,"isLocked":true}
			   ]}
			]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v0/searches/s1":
			// searchOnce deletes the search once its responses are harvested
			// after completion — see TestSearchDeletesSearchAfterHarvest for the
			// ordering assertion; this test only needs the request handled.
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	got, err := c.Search(context.Background(), "artist album", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if polls == 0 {
		t.Error("expected at least one poll of the search state")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result (locked file skipped), got %d", len(got))
	}
	if got[0].Username != "bob" || got[0].BitRate != 900 {
		t.Errorf("unexpected result: %+v", got[0])
	}
	if !got[0].HasFreeUploadSlot || got[0].UploadSpeed != 1000 {
		t.Errorf("per-user reliability fields not propagated: %+v", got[0])
	}
}

// TestSearchDeletesSearchAfterHarvest pins that a completed search is deleted
// from slskd only AFTER its responses are harvested — never before, since
// deleting first would race slskd's own async finalize (see searchOnce's doc
// comment).
func TestSearchDeletesSearchAfterHarvest(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seq = append(seq, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,
			  "files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v0/searches/s1":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	got, err := c.Search(context.Background(), "artist album", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}

	mu.Lock()
	defer mu.Unlock()
	responsesIdx, deleteIdx := -1, -1
	for i, s := range seq {
		switch s {
		case "GET /api/v0/searches/s1/responses":
			if responsesIdx == -1 {
				responsesIdx = i
			}
		case "DELETE /api/v0/searches/s1":
			if deleteIdx == -1 {
				deleteIdx = i
			}
		}
	}
	if deleteIdx == -1 {
		t.Fatalf("expected the search to be deleted after harvest, sequence: %v", seq)
	}
	if responsesIdx == -1 || deleteIdx < responsesIdx {
		t.Errorf("expected DELETE to happen after harvesting responses, sequence: %v", seq)
	}
}

func TestSearchRetriesOnEmptyResults(t *testing.T) {
	var posts, respCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			posts++
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			respCalls++
			// slskd's search DB intermittently drops all responses: first search
			// comes back empty, a retry of the same query succeeds.
			if respCalls == 1 {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,
				  "files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
			}
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = time.Millisecond
	c.searchBackoff = time.Millisecond
	got, err := c.Search(context.Background(), "q", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected retry to recover results, got %d", len(got))
	}
	if posts < 2 {
		t.Errorf("expected a second search after an empty result, posts=%d", posts)
	}
}

func TestSearchGivesUpAfterRetriesWhenEmpty(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			posts++
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[]`)) // genuinely no matches: stays empty every attempt
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = time.Millisecond
	c.searchBackoff = time.Millisecond
	got, err := c.Search(context.Background(), "q", time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d", len(got))
	}
	// Default is 2 retries → 3 total attempts, then give up (assume no matches).
	if posts != 3 {
		t.Errorf("expected 3 total search attempts, got %d", posts)
	}
}

func TestSearchReturnsPartialOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			// never completes -> forces the timeout path
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,"files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	c.stopGrace = 25 * time.Millisecond // fake never reports isComplete; don't wait out the real grace
	got, err := c.Search(context.Background(), "q", 30*time.Millisecond)
	if err != nil {
		t.Fatalf("timeout should return partial results, not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected partial results on timeout, got %d", len(got))
	}
}

// TestSearchTimeoutDoesNotDeleteSearchInGraceFallback pins the safety-critical
// branch of stopAndHarvest: when a search never reports isComplete (even after
// being asked to stop) and the stopGrace budget expires, the fallback harvests
// best-effort but must NOT delete the search — deleting mid-finalize is the
// same async-write race that used to drop responses (see stopAndHarvest's doc
// comment). Only searchOnce/stopAndHarvest's isComplete branches are allowed
// to delete.
func TestSearchTimeoutDoesNotDeleteSearchInGraceFallback(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seq = append(seq, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v0/searches/s1":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			// Never completes, even after the stop PUT: forces the grace-expiry
			// fallback in stopAndHarvest.
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,"files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = 20 * time.Millisecond
	c.stopGrace = 20 * time.Millisecond
	got, err := c.Search(context.Background(), "q", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("grace-expiry fallback should return partial results, not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected partial results from the fallback harvest, got %d", len(got))
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range seq {
		if s == "DELETE /api/v0/searches/s1" {
			t.Fatalf("search must not be deleted when isComplete was never observed, sequence: %v", seq)
		}
	}
}

func TestSearchCleanupFallbackIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v0/searches/s1":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			<-r.Context().Done() // the final harvest must cancel this request
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	c.stopGrace = 20 * time.Millisecond
	c.searchRetries = 0
	started := time.Now()
	_, err := c.Search(context.Background(), "q", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected the deliberately blocked final harvest to fail")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded cleanup took %v", elapsed)
	}
}

func TestStopAndHarvestUsesOneTotalCleanupDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			<-r.Context().Done()
		default:
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	c.stopGrace = 200 * time.Millisecond
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	_, err := c.stopAndHarvest(parent, "s1")
	if err == nil {
		t.Fatal("expected the blocked fallback harvest to fail")
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("cleanup restarted its deadline between phases: elapsed %v for %v total budget", elapsed, c.stopGrace)
	}
}

// TestSearchStopsIncompleteSearchToHarvestPartial pins the fix for a live bug:
// slskd only persists a search's responses when the search is finalized
// (isComplete), so harvesting /responses from a still-InProgress search
// returns an empty list even when responseCount is already large. Verified
// against a live slskd: at t=20s state was InProgress with responseCount=42
// while /responses returned 0 groups; the moment the search completed,
// /responses returned everything. The client must therefore STOP the search
// (PUT /api/v0/searches/{id}) when its own timeout fires, wait for slskd to
// finalize, and only then harvest.
func TestSearchStopsIncompleteSearchToHarvestPartial(t *testing.T) {
	var mu sync.Mutex
	stopped := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v0/searches/s1":
			stopped = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
			// Completes only once the client has asked slskd to stop it.
			json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed, Cancelled", "isComplete": stopped})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
			// Real slskd: empty until the search is finalized.
			if !stopped {
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,"files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.pollInterval = 5 * time.Millisecond
	c.stopGrace = 500 * time.Millisecond
	c.searchRetries = 0 // a single attempt: the stop-and-harvest path must succeed on its own
	got, err := c.Search(context.Background(), "q", 30*time.Millisecond)
	if err != nil {
		t.Fatalf("timeout should stop the search and return partial results, not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected harvested results after stopping the search, got %d", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	if !stopped {
		t.Fatal("client never asked slskd to stop the search (PUT /api/v0/searches/s1)")
	}
}

// TestToCoreMapsAttributesIncludingJSONNull verifies result.toCore maps the
// new nullable attribute fields (issue #58), and that JSON `null` for
// sampleRate/bitDepth decodes as a no-op leaving the Go int fields at zero —
// exactly core.SearchResult's "unknown" semantics, no pointer needed.
func TestToCoreMapsAttributesIncludingJSONNull(t *testing.T) {
	var r result
	if err := json.Unmarshal([]byte(`{
		"username":"bob","filename":"a.flac","size":1,"bitRate":320,
		"length":245,"sampleRate":null,"bitDepth":null,"isVariableBitRate":true
	}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := r.toCore()
	want := core.SearchResult{
		Username: "bob", Filename: "a.flac", Size: 1, BitRate: 320,
		Duration: 245, SampleRate: 0, BitDepth: 0, VariableBitRate: true,
	}
	if got != want {
		t.Fatalf("toCore = %+v, want %+v", got, want)
	}
}

// TestToCoreMapsAttributesAllPresent covers the non-null path for
// length/sampleRate/bitDepth together.
func TestToCoreMapsAttributesAllPresent(t *testing.T) {
	r := result{
		Username: "bob", Filename: "a.flac", Size: 1, BitRate: 320,
		Length: 245, SampleRate: 44100, BitDepth: 16, IsVariableBitRate: false,
	}
	got := r.toCore()
	want := core.SearchResult{
		Username: "bob", Filename: "a.flac", Size: 1, BitRate: 320,
		Duration: 245, SampleRate: 44100, BitDepth: 16, VariableBitRate: false,
	}
	if got != want {
		t.Fatalf("toCore = %+v, want %+v", got, want)
	}
}

// TestSearchStreamDelegatesToSearch pins the two properties SearchStream gets
// for free by delegating to Search instead of re-implementing the POST → poll
// → isComplete → harvest → delete sequence (issue #58 review):
//
//   - "inherits the empty-result retry": a first attempt that finishes with
//     zero responses must be retried, exactly as Search documents. An
//     independent SearchStream implementation without the retry reports "no
//     hits" here, and the manual-search UI then tells the user nobody on the
//     network is sharing the album — which is false.
//   - "preserves delete-after-harvest ordering": the search is DELETEd only
//     after its responses are in hand (the "affected 0 rows" race recorded on
//     stopAndHarvest), and everything arrives as exactly one emit call, since
//     slskd cannot stream (see SearchStreaming).
func TestSearchStreamDelegatesToSearch(t *testing.T) {
	tests := []struct {
		name           string
		emptyAttempts  int // how many search attempts return zero responses
		wantBatches    int
		wantPOSTCount  int
		wantFilenames  []string
		wantDeleteLast bool
	}{
		{
			name:           "inherits the empty-result retry",
			emptyAttempts:  1,
			wantBatches:    1,
			wantPOSTCount:  2,
			wantFilenames:  []string{"a.flac"},
			wantDeleteLast: true,
		},
		{
			name:           "preserves delete-after-harvest ordering",
			emptyAttempts:  0,
			wantBatches:    1,
			wantPOSTCount:  1,
			wantFilenames:  []string{"a.flac"},
			wantDeleteLast: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var seq []string
			posts := 0

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seq = append(seq, r.Method+" "+r.URL.Path)
				if r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches" {
					posts++
				}
				attempt := posts
				mu.Unlock()

				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
					json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1":
					json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "Completed", "isComplete": true})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches/s1/responses":
					if attempt <= tt.emptyAttempts {
						w.Write([]byte(`[]`))
						return
					}
					w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"queueLength":0,"uploadSpeed":1,"files":[{"filename":"a.flac","size":1,"bitRate":900,"isLocked":false}]}]`))
				case r.Method == http.MethodDelete && r.URL.Path == "/api/v0/searches/s1":
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()

			c := New(srv.URL, "k")
			c.pollInterval = 5 * time.Millisecond
			c.searchBackoff = time.Millisecond

			var batches [][]core.SearchResult
			emit := func(batch []core.SearchResult) { batches = append(batches, batch) }
			if err := c.SearchStream(context.Background(), "q", time.Second, emit); err != nil {
				t.Fatalf("SearchStream: %v", err)
			}

			if len(batches) != tt.wantBatches {
				t.Fatalf("emit called %d times, want %d (slskd delivers one batch at completion)", len(batches), tt.wantBatches)
			}
			var got []string
			for _, b := range batches {
				for _, r := range b {
					got = append(got, r.Filename)
				}
			}
			if !slices.Equal(got, tt.wantFilenames) {
				t.Fatalf("emitted %v, want %v", got, tt.wantFilenames)
			}

			mu.Lock()
			defer mu.Unlock()
			if posts != tt.wantPOSTCount {
				t.Fatalf("POST /api/v0/searches happened %d times, want %d: %v", posts, tt.wantPOSTCount, seq)
			}
			if tt.wantDeleteLast {
				// Checked per attempt, not across the whole sequence: a retry
				// legitimately produces POST, harvest, DELETE, POST, harvest,
				// DELETE, so a global "first delete after last harvest" test
				// would be wrong. What must hold is that no attempt ever
				// deletes its search before harvesting it.
				harvested, deletes := false, 0
				for _, s := range seq {
					switch s {
					case "POST /api/v0/searches":
						harvested = false
					case "GET /api/v0/searches/s1/responses":
						harvested = true
					case "DELETE /api/v0/searches/s1":
						if !harvested {
							t.Fatalf("delete happened before this attempt harvested its responses: %v", seq)
						}
						deletes++
					}
				}
				if deletes == 0 {
					t.Fatalf("search was never deleted: %v", seq)
				}
			}
		})
	}
}

func TestSearchStreamingReportsFalseForSlskdBackend(t *testing.T) {
	c := New("http://unused", "k")
	if c.SearchStreaming() {
		t.Fatal("slskd client must report SearchStreaming() == false")
	}
}
