# Discovery Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Activate the dormant `AlbumJob` state machine: discover wanted Lidarr albums, search Soulseek via slskd, pick a quality-floored candidate, download it via the v1 write-ahead/reconciler, and hand off to Lidarr via the manual-import API with rejection handling.

**Architecture:** A second engine loop (`Discoverer.RunOnce`, ticking on `LidarrPoll`) drives each `AlbumJob` one state-transition per tick, state living in SQLite. It reuses v1's store write-ahead methods and reconciler (which keeps monitoring transfers). New pieces: slskd async `Search`, Lidarr manual-import methods, a candidate quality floor + reliability scoring, ~8 new store queries, and the discovery state machine itself.

**Tech Stack:** Go 1.26, existing `internal/*` packages, stdlib `net/http`/`net/http/httptest`, `modernc.org/sqlite`, `log/slog`.

## Global Constraints

- Go module path: `github.com/samuelenocsson/slusk`. Go 1.26. No cgo.
- All exported functions/methods get a doc comment.
- `store` is the only package that runs SQL; `slskd`/`lidarr` mirror their APIs and import no internal package except via return types; `engine` owns its consumer-defined port interfaces.
- Prose in this plan is Swedish context; all code, identifiers, paths, and commit messages are English.
- **VERIFIED real API shapes (2026-07-01, against Lidarr 3.1.0 + slskd 0.25.1):**
  - slskd `POST /api/v0/searches` body `{"searchText": q}` → 200 `{"id": <guid>, "state": "InProgress", "isComplete": false, ...}`.
  - slskd `GET /api/v0/searches/{id}` → the search object with `"isComplete": bool` and `"state": string`.
  - slskd `GET /api/v0/searches/{id}/responses` → array grouped by user: `[{"username", "hasFreeUploadSlot": bool, "queueLength": int, "uploadSpeed": int, "files": [{"filename", "size", "bitRate", "isLocked": bool}]}]`. Filenames use `\` separators with a `@@user\` prefix.
  - slskd `DELETE /api/v0/searches/{id}` → 204 (cleanup).
  - slskd download dir = `/music/slskd-downloads`; **Lidarr sees it at the SAME path** (manualimport on that folder returns 200). No cross-container path translation needed.
  - Lidarr `GET /api/v1/manualimport?folder=<path>` → 200 array of items, each with `id`, `path`, `name`, `rejections` (array; empty = importable), and album/quality fields. Empty folder → `[]`.
  - Lidarr `POST /api/v1/command {"name":"ManualImport","importMode":"move","files":[{"path","folderName","...ids"}]}` triggers the import.
- Quality floor default: lossless always accepted; lossy accepted only if `bitRate >= min_bitrate` (default 192). Locked files (`isLocked`) are always skipped.

---

### Task 1: slskd async Search + richer result type

**Files:**
- Modify: `internal/slskd/client.go`
- Test: `internal/slskd/client_test.go`

**Interfaces:**
- Consumes: existing `slskd.Client`, `slskd.Result`.
- Produces: extended `slskd.Result` with `IsLocked bool`, `HasFreeUploadSlot bool`, `QueueLength int`, `UploadSpeed int`; new method `(*Client) Search(ctx context.Context, query string, timeout time.Duration) ([]Result, error)`. `New` still `New(baseURL, apiKey string) *Client`; add an unexported `pollInterval time.Duration` field defaulting to `1*time.Second`.

- [ ] **Step 1: Extend the Result type and add a poll-interval field**

In `internal/slskd/client.go`, replace the `Result` struct with:
```go
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
```
Add a `pollInterval time.Duration` field to `Client`, and set it in `New`:
```go
func New(baseURL, apiKey string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}, pollInterval: time.Second}
}
```

- [ ] **Step 2: Write the failing test**

Add to `internal/slskd/client_test.go`:
```go
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
		case r.Method == http.MethodDelete:
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/slskd/ -run TestSearch`
Expected: FAIL — `c.Search` undefined.

- [ ] **Step 4: Implement Search**

Add to `internal/slskd/client.go` (keep existing imports; ensure `context`, `time` present):
```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/slskd/`
Expected: PASS (existing + new).

- [ ] **Step 6: Commit**

```bash
git add internal/slskd/
git commit -m "feat(slskd): async Search with reliability signals and locked-file skip"
```

---

### Task 2: Lidarr manual-import + TrackCount

**Files:**
- Modify: `internal/lidarr/client.go`
- Test: `internal/lidarr/client_test.go`

**Interfaces:**
- Produces: `lidarr.ManualImportItem{ID int64; Path string; FolderName string; ArtistID int64; AlbumID int64; Rejections []string; Importable bool}`; methods `(*Client) ManualImportCandidates(ctx, folder string) ([]ManualImportItem, error)`, `(*Client) ExecuteManualImport(ctx, items []ManualImportItem) error`; extended `WantedAlbum` with `TrackCount int`.

- [ ] **Step 1: Write the failing test**

Add to `internal/lidarr/client_test.go`:
```go
func TestManualImportCandidatesParsesRejections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/manualimport" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`[
		  {"id":1,"path":"/music/slskd-downloads/A/01.flac","folderName":"A","artistId":5,"albumId":9,"rejections":[]},
		  {"id":2,"path":"/music/slskd-downloads/A/02.mp3","folderName":"A","artistId":5,"albumId":9,
		   "rejections":[{"reason":"Quality Unknown not in profile","type":"permanent"}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	items, err := c.ManualImportCandidates(context.Background(), "/music/slskd-downloads/A")
	if err != nil {
		t.Fatalf("ManualImportCandidates: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if !items[0].Importable {
		t.Errorf("item 0 has no rejections, should be importable")
	}
	if items[1].Importable {
		t.Errorf("item 1 has a rejection, should not be importable")
	}
	if items[1].Rejections[0] != "Quality Unknown not in profile" {
		t.Errorf("rejection reason not parsed: %v", items[1].Rejections)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lidarr/ -run TestManualImport`
Expected: FAIL — `ManualImportCandidates` undefined.

- [ ] **Step 3: Implement the methods and extend WantedAlbum**

In `internal/lidarr/client.go`, add `TrackCount int` to `WantedAlbum`, and in `WantedMissing` populate it from the record's `statistics.trackCount` (extend the anonymous decode struct with `Statistics struct{ TrackCount int \`json:"trackCount"\` } \`json:"statistics"\`` and set `TrackCount: r.Statistics.TrackCount`).

Then add:
```go
// ManualImportItem is one file Lidarr found in a folder, with any import rejections.
type ManualImportItem struct {
	ID         int64
	Path       string
	FolderName string
	ArtistID   int64
	AlbumID    int64
	Rejections []string
	Importable bool // true when Rejections is empty
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
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		FolderName string `json:"folderName"`
		ArtistID   int64  `json:"artistId"`
		AlbumID    int64  `json:"albumId"`
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
		out = append(out, ManualImportItem{
			ID: it.ID, Path: it.Path, FolderName: it.FolderName,
			ArtistID: it.ArtistID, AlbumID: it.AlbumID,
			Rejections: reasons, Importable: len(reasons) == 0,
		})
	}
	return out, nil
}

// ExecuteManualImport tells Lidarr to import the given items (move mode).
func (c *Client) ExecuteManualImport(ctx context.Context, items []ManualImportItem) error {
	files := make([]map[string]any, 0, len(items))
	for _, it := range items {
		files = append(files, map[string]any{
			"path": it.Path, "folderName": it.FolderName,
			"artistId": it.ArtistID, "albumId": it.AlbumID,
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
```
Ensure `net/url` is imported.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lidarr/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lidarr/
git commit -m "feat(lidarr): manual-import candidates + execute, WantedAlbum.TrackCount"
```

---

### Task 3: Config additions for the pipeline

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/testdata/valid.toml`

**Interfaces:**
- Produces: `EngineConfig` gains `SearchTimeout Duration`, `MinBitrate int`, `MaxConcurrentSearches int`, `CandidateBackoff Duration`, `FailedRetryAfter Duration`; new `PathsConfig{ SlskdCompleteDir string }` on `Config` as field `Paths`. `Validate` rejects the new required fields.

- [ ] **Step 1: Update the valid fixture and write the failing test**

In `internal/config/testdata/valid.toml`, add under `[engine]`:
```toml
search_timeout = "30s"
min_bitrate = 192
max_concurrent_searches = 2
candidate_backoff = "10m"
failed_retry_after = "24h"
```
and add a new section:
```toml
[paths]
slskd_complete_dir = "/music/slskd-downloads"
```

Add to `internal/config/config_test.go`:
```go
func TestLoadValidHasPipelineFields(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Engine.MinBitrate != 192 {
		t.Errorf("MinBitrate = %d", cfg.Engine.MinBitrate)
	}
	if cfg.Engine.SearchTimeout.Duration.Seconds() != 30 {
		t.Errorf("SearchTimeout = %v", cfg.Engine.SearchTimeout.Duration)
	}
	if cfg.Paths.SlskdCompleteDir != "/music/slskd-downloads" {
		t.Errorf("SlskdCompleteDir = %q", cfg.Paths.SlskdCompleteDir)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadValidHasPipeline`
Expected: FAIL — fields don't exist (won't compile).

- [ ] **Step 3: Add the fields and validation**

In `internal/config/config.go`, extend `EngineConfig`:
```go
type EngineConfig struct {
	MaxCandidatesPerAlbum int      `toml:"max_candidates_per_album"`
	TransferDeadline      Duration `toml:"transfer_deadline"`
	StallTimeout          Duration `toml:"stall_timeout"`
	SearchTimeout         Duration `toml:"search_timeout"`
	MinBitrate            int      `toml:"min_bitrate"`
	MaxConcurrentSearches int      `toml:"max_concurrent_searches"`
	CandidateBackoff      Duration `toml:"candidate_backoff"`
	FailedRetryAfter      Duration `toml:"failed_retry_after"`
	Weights               Weights  `toml:"weights"`
}
```
Add a paths section type and field on `Config`:
```go
// PathsConfig holds filesystem paths shared with the arr-stack.
type PathsConfig struct {
	SlskdCompleteDir string `toml:"slskd_complete_dir"`
}
```
Add `Paths PathsConfig \`toml:"paths"\`` to the `Config` struct.
In `Validate`, add:
```go
	if c.Engine.SearchTimeout.Duration <= 0 {
		problems = append(problems, "engine.search_timeout must be > 0")
	}
	if c.Engine.MinBitrate <= 0 {
		problems = append(problems, "engine.min_bitrate must be > 0")
	}
	if c.Engine.MaxConcurrentSearches <= 0 {
		problems = append(problems, "engine.max_concurrent_searches must be > 0")
	}
	if c.Engine.CandidateBackoff.Duration <= 0 {
		problems = append(problems, "engine.candidate_backoff must be > 0")
	}
	if c.Engine.FailedRetryAfter.Duration <= 0 {
		problems = append(problems, "engine.failed_retry_after must be > 0")
	}
	if c.Paths.SlskdCompleteDir == "" {
		problems = append(problems, "paths.slskd_complete_dir is required")
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (all, including the existing `TestLoadValid`).

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): pipeline settings (search, quality floor, paths, backoff)"
```

---

### Task 4: Matcher quality floor + reliability scoring

**Files:**
- Modify: `internal/matcher/matcher.go`
- Test: `internal/matcher/matcher_test.go`

**Interfaces:**
- Consumes: `config.Weights`, extended `slskd.Result` (with `HasFreeUploadSlot`, `QueueLength`, `UploadSpeed`).
- Produces: `NewWeighted(w config.Weights, minBitrate int) Scorer` (SIGNATURE CHANGE — now takes `minBitrate`); `Rank` filters files below the quality floor and folds the reliability signal into the score. `Candidate` unchanged.

- [ ] **Step 1: Write the failing tests**

Add to `internal/matcher/matcher_test.go`:
```go
func TestRankDropsBelowBitrateFloor(t *testing.T) {
	s := NewWeighted(config.Weights{Format: 1, Bitrate: 1, FileCount: 1}, 192)
	results := []slskd.Result{
		{Username: "lowmp3", Filename: "a.mp3", BitRate: 128}, // below floor -> dropped
		{Username: "flac", Filename: "a.flac", BitRate: 0},    // lossless -> kept even with 0 bitrate
	}
	ranked := s.Rank(results)
	for _, c := range ranked {
		if c.Username == "lowmp3" {
			t.Errorf("128kbps mp3 should be dropped by the 192 floor")
		}
	}
	if len(ranked) != 1 || ranked[0].Username != "flac" {
		t.Fatalf("expected only the flac candidate, got %+v", ranked)
	}
}

func TestRankRewardsReliableUploader(t *testing.T) {
	s := NewWeighted(config.Weights{Format: 1, Bitrate: 0, Reliability: 10, FileCount: 1}, 192)
	results := []slskd.Result{
		{Username: "busy", Filename: "a.flac", BitRate: 900, HasFreeUploadSlot: false, QueueLength: 20},
		{Username: "free", Filename: "a.flac", BitRate: 900, HasFreeUploadSlot: true, QueueLength: 0},
	}
	ranked := s.Rank(results)
	if ranked[0].Username != "free" {
		t.Errorf("free-slot uploader should rank first, got %q", ranked[0].Username)
	}
}
```
(These replace reliance on the old two-arg `NewWeighted`; update the existing tests in this file to pass `192` as the new second arg, e.g. `NewWeighted(w, 192)`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/matcher/`
Expected: FAIL — `NewWeighted` arity changed / new behavior absent.

- [ ] **Step 3: Implement the floor and reliability**

In `internal/matcher/matcher.go`, change the constructor and scorer:
```go
// NewWeighted returns a Scorer that drops files below the quality floor (lossless
// always kept; lossy kept only if bitRate >= minBitrate) and scores the rest by
// format, bitrate, file count, and the peer's upload reliability.
func NewWeighted(w config.Weights, minBitrate int) Scorer {
	return &weighted{w: w, minBitrate: minBitrate}
}

type weighted struct {
	w          config.Weights
	minBitrate int
}

// passesFloor reports whether a file meets the quality floor. Lossless formats
// (score 1.0) are always kept; lossy files need bitRate >= minBitrate.
func (x *weighted) passesFloor(r slskd.Result) bool {
	if formatScore(r.Filename) >= 1.0 {
		return true
	}
	return r.BitRate >= x.minBitrate
}

// reliabilityScore maps a peer's upload signals to a 0..1 factor.
func reliabilityScore(r slskd.Result) float64 {
	score := 0.0
	if r.HasFreeUploadSlot {
		score += 0.7
	}
	if r.QueueLength == 0 {
		score += 0.3
	}
	return score
}
```
Then in `Rank`, before grouping, drop files that fail the floor, and add the reliability term to the per-user score:
```go
func (x *weighted) Rank(results []slskd.Result) []Candidate {
	byUser := map[string][]slskd.Result{}
	for _, r := range results {
		if !x.passesFloor(r) {
			continue
		}
		byUser[r.Username] = append(byUser[r.Username], r)
	}
	var candidates []Candidate
	for user, files := range byUser {
		var score float64
		for _, f := range files {
			score += x.w.Format * formatScore(f.Filename)
			score += x.w.Bitrate * (float64(f.BitRate) / 1000.0)
		}
		score += x.w.FileCount * float64(len(files))
		score += x.w.Reliability * reliabilityScore(files[0]) // per-user, same across files
		candidates = append(candidates, Candidate{Username: user, Files: files, Score: score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Username < candidates[j].Username
	})
	return candidates
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/matcher/`
Expected: PASS (updated existing + 2 new).

- [ ] **Step 5: Commit**

```bash
git add internal/matcher/
git commit -m "feat(matcher): quality floor filter and reliability scoring"
```

---

### Task 5: Store — pipeline query/transition methods

**Files:**
- Create: `internal/store/pipeline.go`
- Test: `internal/store/pipeline_test.go`

**Interfaces:**
- Consumes: `core` types, `store.Store`, existing `UpsertDiscoveredJob`/`CreateAttempt`/`RecordEnqueueIntent`.
- Produces on `*Store`:
  - `JobsInState(ctx, state core.AlbumJobState, limit int) ([]core.AlbumJob, error)`
  - `DueCooldownJobs(ctx, now time.Time, limit int) ([]core.AlbumJob, error)` — `state=COOLDOWN AND next_attempt_at <= now`
  - `AttemptsForJob(ctx, jobID int64) ([]core.CandidateAttempt, error)`
  - `TransfersForAttempt(ctx, attemptID int64) ([]core.Transfer, error)`
  - `FailAttempt(ctx, attemptID int64, reason string, backoffUntil, now time.Time) error`
  - `SucceedAttempt(ctx, attemptID int64, now time.Time) error`
  - `SetJobCooldown(ctx, jobID int64, nextAttemptAt, now time.Time) error` — sets state=COOLDOWN, next_attempt_at
  - `IncrementCandidatesTried(ctx, jobID int64, now time.Time) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/store/pipeline_test.go`:
```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

func TestJobsInStateAndCooldown(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 100, now)

	inState, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(inState) != 1 || inState[0].ID != job.ID {
		t.Fatalf("expected the discovered job, got %+v", inState)
	}

	// Put it in cooldown with next_attempt in the past -> due.
	if err := s.SetJobCooldown(ctx, job.ID, now.Add(-time.Minute), now); err != nil {
		t.Fatalf("SetJobCooldown: %v", err)
	}
	due, err := s.DueCooldownJobs(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueCooldownJobs: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due cooldown job, got %d", len(due))
	}
	// And not due if next_attempt is in the future.
	_ = s.SetJobCooldown(ctx, job.ID, now.Add(time.Hour), now)
	future, _ := s.DueCooldownJobs(ctx, now, 10)
	if len(future) != 0 {
		t.Errorf("job with future next_attempt should not be due")
	}
}

func TestAttemptsAndTransfersForJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 200, now)
	attemptID, _ := s.CreateAttempt(ctx, job.ID, "bob", 2.0, now)
	_, _ = s.RecordEnqueueIntent(ctx, attemptID, "bob", "f.flac", now.Add(time.Hour), now)

	attempts, err := s.AttemptsForJob(ctx, job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Username != "bob" {
		t.Fatalf("AttemptsForJob: %v %+v", err, attempts)
	}
	transfers, err := s.TransfersForAttempt(ctx, attemptID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForAttempt: %v %+v", err, transfers)
	}

	if err := s.FailAttempt(ctx, attemptID, "timeout", now.Add(10*time.Minute), now); err != nil {
		t.Fatalf("FailAttempt: %v", err)
	}
	after, _ := s.AttemptsForJob(ctx, job.ID)
	if after[0].State != "FAILED" || after[0].FailReason != "timeout" {
		t.Errorf("attempt not marked failed: %+v", after[0])
	}
	if err := s.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
		t.Fatalf("IncrementCandidatesTried: %v", err)
	}
	jobs, _ := s.JobsInState(ctx, core.StateDiscovered, 10)
	if len(jobs) != 1 || jobs[0].CandidatesTried != 1 {
		t.Errorf("candidates_tried not incremented: %+v", jobs)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestJobsInState|TestAttempts'`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement the methods**

Create `internal/store/pipeline.go`:
```go
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

const jobSelect = `SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at FROM album_jobs`

func scanJobs(rows *sql.Rows) ([]core.AlbumJob, error) {
	var out []core.AlbumJob
	for rows.Next() {
		var j core.AlbumJob
		var state string
		if err := rows.Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.State = core.AlbumJobState(state)
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobsInState returns up to limit jobs currently in the given state.
func (s *Store) JobsInState(ctx context.Context, state core.AlbumJobState, limit int) ([]core.AlbumJob, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE state = ? ORDER BY updated_at LIMIT ?`, string(state), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// DueCooldownJobs returns up to limit COOLDOWN jobs whose next_attempt_at has passed.
func (s *Store) DueCooldownJobs(ctx context.Context, now time.Time, limit int) ([]core.AlbumJob, error) {
	rows, err := s.db.QueryContext(ctx,
		jobSelect+` WHERE state = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT ?`,
		string(core.StateCooldown), now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// AttemptsForJob returns all candidate attempts for a job, oldest first.
func (s *Store) AttemptsForJob(ctx context.Context, jobID int64) ([]core.CandidateAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, album_job_id, username, score, state, fail_reason, backoff_until, created_at
		 FROM candidate_attempts WHERE album_job_id = ? ORDER BY created_at`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.CandidateAttempt
	for rows.Next() {
		var a core.CandidateAttempt
		if err := rows.Scan(&a.ID, &a.AlbumJobID, &a.Username, &a.Score, &a.State, &a.FailReason, &a.BackoffUntil, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TransfersForAttempt returns all transfers belonging to a candidate attempt.
func (s *Store) TransfersForAttempt(ctx context.Context, attemptID int64) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx, transferSelect+` WHERE attempt_id = ?`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// FailAttempt marks a candidate attempt FAILED with a reason and a backoff time.
func (s *Store) FailAttempt(ctx context.Context, attemptID int64, reason string, backoffUntil, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidate_attempts SET state = 'FAILED', fail_reason = ?, backoff_until = ? WHERE id = ?`,
		reason, backoffUntil, attemptID)
	if err != nil {
		return fmt.Errorf("fail attempt: %w", err)
	}
	return nil
}

// SucceedAttempt marks a candidate attempt SUCCEEDED.
func (s *Store) SucceedAttempt(ctx context.Context, attemptID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidate_attempts SET state = 'SUCCEEDED' WHERE id = ?`, attemptID)
	if err != nil {
		return fmt.Errorf("succeed attempt: %w", err)
	}
	return nil
}

// SetJobCooldown moves a job to COOLDOWN with the given next-attempt time.
func (s *Store) SetJobCooldown(ctx context.Context, jobID int64, nextAttemptAt, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = ?, next_attempt_at = ?, updated_at = ? WHERE id = ?`,
		string(core.StateCooldown), nextAttemptAt, now, jobID)
	if err != nil {
		return fmt.Errorf("set job cooldown: %w", err)
	}
	return nil
}

// IncrementCandidatesTried bumps the count of candidates tried for a job.
func (s *Store) IncrementCandidatesTried(ctx context.Context, jobID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET candidates_tried = candidates_tried + 1, updated_at = ? WHERE id = ?`,
		now, jobID)
	if err != nil {
		return fmt.Errorf("increment candidates tried: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/`
Expected: PASS (all).

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): pipeline job/attempt query and transition methods"
```

---

### Task 6: Engine — discovery ports + RecordEnqueueIntent conflict-safety

**Files:**
- Modify: `internal/engine/ports.go`
- Modify: `internal/store/jobs.go` (make `RecordEnqueueIntent` upsert-safe)
- Test: `internal/store/jobs_test.go`

**Interfaces:**
- Produces: engine ports `MusicSource` and `Searcher`/`Enqueuer` (see below); `RecordEnqueueIntent` becomes idempotent on the `(username, filename)` unique key (fixes the v1 downstream note: a re-enqueue across attempts must not error).

- [ ] **Step 1: Write the failing test for conflict-safe enqueue**

Add to `internal/store/jobs_test.go`:
```go
func TestRecordEnqueueIntentIsConflictSafe(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 300, now)
	a1, _ := s.CreateAttempt(ctx, job.ID, "bob", 1.0, now)

	id1, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("first intent: %v", err)
	}
	// A later attempt re-enqueues the same (username, filename) — must not error,
	// and must return the existing row rather than creating a duplicate.
	id2, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(2*time.Hour), now)
	if err != nil {
		t.Fatalf("second intent (conflict) errored: %v", err)
	}
	if id1 != id2 {
		t.Errorf("expected same transfer row on conflict, got %d and %d", id1, id2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestRecordEnqueueIntentIsConflictSafe`
Expected: FAIL — a duplicate insert hits the UNIQUE constraint and errors.

- [ ] **Step 3: Make RecordEnqueueIntent conflict-safe**

In `internal/store/jobs.go`, replace `RecordEnqueueIntent` with an upsert that returns the existing/updated row id:
```go
// RecordEnqueueIntent is step 1 of the write-ahead enqueue: it persists a QUEUED
// transfer with no slskd id BEFORE slskd is called. It is idempotent on the
// (username, filename) key: a re-enqueue updates the existing row's attempt and
// deadline and returns that row, rather than violating the UNIQUE constraint.
func (s *Store) RecordEnqueueIntent(ctx context.Context, attemptID int64, username, filename string, deadline, now time.Time) (int64, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transfers (attempt_id, username, filename, state, deadline, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(username, filename) DO UPDATE SET
		   attempt_id = excluded.attempt_id,
		   state = excluded.state,
		   deadline = excluded.deadline,
		   slskd_id = '',
		   bytes_done = 0,
		   updated_at = excluded.updated_at`,
		attemptID, username, filename, string(core.TransferQueued), deadline, now)
	if err != nil {
		return 0, fmt.Errorf("upsert transfer intent: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM transfers WHERE username = ? AND filename = ?`, username, filename).Scan(&id); err != nil {
		return 0, fmt.Errorf("read transfer id: %w", err)
	}
	return id, nil
}
```

- [ ] **Step 4: Run the store tests**

Run: `go test ./internal/store/`
Expected: PASS (all, including the existing write-ahead test and the new conflict test).

- [ ] **Step 5: Add the discovery ports**

In `internal/engine/ports.go`, add (keep existing `PeerNetwork`/`JobStore`; extend as shown):
```go
import (
	// ...existing...
	"github.com/samuelenocsson/slusk/internal/lidarr"
	"github.com/samuelenocsson/slusk/internal/matcher"
)

// MusicSource is the slice of the Lidarr client the discoverer needs.
type MusicSource interface {
	WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error)
	ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error)
	ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error
}

// PeerSearcher is the slice of the slskd client the discoverer needs for search+enqueue.
type PeerSearcher interface {
	Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error)
	Enqueue(ctx context.Context, username, filename string) (string, error)
}

// DiscoveryStore is the slice of the store the discoverer needs.
type DiscoveryStore interface {
	UpsertDiscoveredJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error)
	JobsInState(ctx context.Context, state core.AlbumJobState, limit int) ([]core.AlbumJob, error)
	DueCooldownJobs(ctx context.Context, now time.Time, limit int) ([]core.AlbumJob, error)
	AttemptsForJob(ctx context.Context, jobID int64) ([]core.CandidateAttempt, error)
	TransfersForAttempt(ctx context.Context, attemptID int64) ([]core.Transfer, error)
	CreateAttempt(ctx context.Context, jobID int64, username string, score float64, now time.Time) (int64, error)
	RecordEnqueueIntent(ctx context.Context, attemptID int64, username, filename string, deadline, now time.Time) (int64, error)
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
	AdvanceJobState(ctx context.Context, jobID int64, to core.AlbumJobState, now time.Time) error
	FailAttempt(ctx context.Context, attemptID int64, reason string, backoffUntil, now time.Time) error
	SucceedAttempt(ctx context.Context, attemptID int64, now time.Time) error
	SetJobCooldown(ctx context.Context, jobID int64, nextAttemptAt, now time.Time) error
	IncrementCandidatesTried(ctx context.Context, jobID int64, now time.Time) error
}

// Ranker ranks slskd results into candidates (satisfied by matcher.Scorer).
type Ranker interface {
	Rank(results []slskd.Result) []matcher.Candidate
}
```
(`*store.Store`, `*slskd.Client`, `*lidarr.Client`, and `matcher.Scorer` satisfy these implicitly.)

- [ ] **Step 6: Verify it compiles and commit**

Run: `go build ./... && go test ./internal/store/`
Expected: builds; store tests pass.

```bash
git add internal/engine/ports.go internal/store/
git commit -m "feat(engine,store): discovery ports and conflict-safe enqueue intent"
```

---

### Task 7: Engine — path translation helper

**Files:**
- Create: `internal/engine/paths.go`
- Test: `internal/engine/paths_test.go`

**Interfaces:**
- Produces: `AlbumFolder(completeDir string, filenames []string) string` — computes the local album folder Lidarr should scan, from the downloaded transfers' filenames, translating `\`→`/`; falls back to `completeDir` when no common subfolder is found.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/paths_test.go`:
```go
package engine

import "testing"

func TestAlbumFolder(t *testing.T) {
	complete := "/music/slskd-downloads"
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}
	got := AlbumFolder(complete, files)
	want := "/music/slskd-downloads/music/Sia/1000 Forms of Fear (2014)"
	if got != want {
		t.Errorf("AlbumFolder = %q, want %q", got, want)
	}
}

func TestAlbumFolderFallsBackToRoot(t *testing.T) {
	if got := AlbumFolder("/music/dl", nil); got != "/music/dl" {
		t.Errorf("empty filenames should fall back to root, got %q", got)
	}
	// No common directory -> fall back to root.
	files := []string{`a\1.flac`, `b\2.flac`}
	if got := AlbumFolder("/music/dl", files); got != "/music/dl" {
		t.Errorf("no common dir should fall back to root, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestAlbumFolder`
Expected: FAIL — `AlbumFolder` undefined.

- [ ] **Step 3: Implement AlbumFolder**

Create `internal/engine/paths.go`:
```go
package engine

import (
	"path"
	"strings"
)

// AlbumFolder computes the local folder Lidarr should scan for one album, from
// the downloaded transfers' filenames. slskd filenames use "\" separators and a
// leading directory; the common directory of all files (translated to "/") is
// joined under completeDir. If there is no single common directory, it falls
// back to completeDir so Lidarr scans the whole download root.
func AlbumFolder(completeDir string, filenames []string) string {
	if len(filenames) == 0 {
		return completeDir
	}
	dir := func(f string) string {
		f = strings.ReplaceAll(f, `\`, "/")
		return path.Dir(f)
	}
	common := dir(filenames[0])
	for _, f := range filenames[1:] {
		if dir(f) != common {
			return completeDir // no single album folder -> scan the root
		}
	}
	if common == "." || common == "/" || common == "" {
		return completeDir
	}
	return path.Join(completeDir, common)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/ -run TestAlbumFolder`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/paths.go internal/engine/paths_test.go
git commit -m "feat(engine): album-folder path translation from slskd filenames"
```

---

### Task 8: Engine — the Discoverer state machine

**Files:**
- Create: `internal/engine/discovery.go`
- Test: `internal/engine/discovery_test.go`

**Interfaces:**
- Consumes: `MusicSource`, `PeerSearcher`, `DiscoveryStore`, `Ranker`, `core`, `lidarr`, `slskd`, `matcher`, config values.
- Produces: `Discoverer` with `NewDiscoverer(p DiscovererParams) *Discoverer` and `(*Discoverer) RunOnce(ctx context.Context, now time.Time) error`. `DiscovererParams{ Music MusicSource; Peers PeerSearcher; Store DiscoveryStore; Ranker Ranker; CompleteDir string; SearchTimeout, TransferDeadline, CandidateBackoff, FailedRetryAfter time.Duration; MaxCandidates, Batch int; Logger *slog.Logger }`.

**Design note (transition logic per tick):**
- `RunOnce` = `syncWanted` → `startNewJobs` (DISCOVERED + due COOLDOWN) → `advanceDownloading` → `advanceVerifying` → `advanceImporting`. Each stage claims a bounded batch. Every stage is independent and idempotent so a crash mid-tick is safe.

- [ ] **Step 1: Write the failing tests (the core scenarios)**

Create `internal/engine/discovery_test.go`:
```go
package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/lidarr"
	"github.com/samuelenocsson/slusk/internal/matcher"
	"github.com/samuelenocsson/slusk/internal/slskd"
)

// --- fakes ---

type fakeMusic struct {
	wanted     []lidarr.WantedAlbum
	candidates []lidarr.ManualImportItem
	imported   [][]lidarr.ManualImportItem
}

func (f *fakeMusic) WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error) {
	return f.wanted, nil
}
func (f *fakeMusic) ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error) {
	return f.candidates, nil
}
func (f *fakeMusic) ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error {
	f.imported = append(f.imported, items)
	return nil
}

type fakeSearcher struct {
	results   []slskd.Result
	enqueued  []string
}

func (f *fakeSearcher) Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error) {
	return f.results, nil
}
func (f *fakeSearcher) Enqueue(ctx context.Context, username, filename string) (string, error) {
	f.enqueued = append(f.enqueued, filename)
	return "slskd-" + filename, nil
}

// discoStore is a real store-backed DiscoveryStore for these tests (simpler than a fake).
func newDiscoParams(t *testing.T, music *fakeMusic, peers *fakeSearcher) (DiscovererParams, *discoBackedStore) {
	t.Helper()
	st := newBackedStore(t) // helper returning a *store.Store wrapper; see Step 3 note
	return DiscovererParams{
		Music: music, Peers: peers, Store: st, Ranker: matcher.NewWeighted(defaultWeights(), 192),
		CompleteDir: "/music/slskd-downloads", SearchTimeout: time.Second,
		TransferDeadline: 30 * time.Minute, CandidateBackoff: 10 * time.Minute,
		FailedRetryAfter: 24 * time.Hour, MaxCandidates: 3, Batch: 10,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}, st
}

func TestDiscoverStartsSearchAndEnqueues(t *testing.T) {
	music := &fakeMusic{wanted: []lidarr.WantedAlbum{{ID: 1, Title: "A", ArtistName: "X", TrackCount: 2}}}
	peers := &fakeSearcher{results: []slskd.Result{
		{Username: "bob", Filename: `bob\A\01.flac`, BitRate: 900, HasFreeUploadSlot: true},
		{Username: "bob", Filename: `bob\A\02.flac`, BitRate: 900, HasFreeUploadSlot: true},
	}}
	p, st := newDiscoParams(t, music, peers)
	d := NewDiscoverer(p)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := d.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(peers.enqueued) != 2 {
		t.Fatalf("expected 2 files enqueued, got %d", len(peers.enqueued))
	}
	jobs, _ := st.JobsInState(context.Background(), core.StateDownloading, 10)
	if len(jobs) != 1 {
		t.Errorf("expected job in DOWNLOADING, got %d", len(jobs))
	}
}

func TestDiscoverImportsCleanCandidate(t *testing.T) {
	music := &fakeMusic{candidates: []lidarr.ManualImportItem{
		{ID: 1, Path: "/music/slskd-downloads/A/01.flac", Importable: true},
	}}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Seed a job in VERIFYING with a completed transfer.
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 1 {
		t.Fatalf("expected one ExecuteManualImport call, got %d", len(music.imported))
	}
	jobs, _ := st.JobsInState(ctx, core.StateCompleted, 10)
	if len(jobs) != 1 {
		t.Errorf("expected job COMPLETED after clean import, got %d", len(jobs))
	}
}

func TestDiscoverRejectedImportFailsCandidate(t *testing.T) {
	music := &fakeMusic{candidates: []lidarr.ManualImportItem{
		{ID: 1, Path: "/music/slskd-downloads/A/01.mp3", Rejections: []string{"Quality not in profile"}, Importable: false},
	}}
	peers := &fakeSearcher{}
	p, st := newDiscoParams(t, music, peers)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.seedVerifyingJob(t, now)
	d := NewDiscoverer(p)
	if err := d.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(music.imported) != 0 {
		t.Errorf("must not import a rejected candidate")
	}
	jobs, _ := st.JobsInState(ctx, core.StateCooldown, 10)
	if len(jobs) != 1 {
		t.Errorf("rejected import should put the job in COOLDOWN, got %d", len(jobs))
	}
}
```

**Step 2 note — test helpers:** because the `DiscoveryStore` surface is wide, these tests use a thin real-store wrapper rather than a hand-written fake. Add to `discovery_test.go` a `discoBackedStore` wrapping `*store.Store` (embedding it) plus helpers `newBackedStore(t)` (opens a temp-dir store like `store`'s own tests), `seedVerifyingJob(t, now)` (creates a job, attempt, and a COMPLETED transfer, then `AdvanceJobState(..., StateVerifying)`), `defaultWeights()`, and a `testWriter{t}` that writes log lines to `t.Log`. Implement these with the store's public methods and `UpdateTransferProgress` to set the transfer COMPLETED. Keep them in the test file.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run TestDiscover`
Expected: FAIL — `NewDiscoverer`/`Discoverer`/`DiscovererParams` undefined.

- [ ] **Step 4: Implement the Discoverer**

Create `internal/engine/discovery.go`:
```go
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/lidarr"
)

// DiscovererParams configures a Discoverer.
type DiscovererParams struct {
	Music            MusicSource
	Peers            PeerSearcher
	Store            DiscoveryStore
	Ranker           Ranker
	CompleteDir      string
	SearchTimeout    time.Duration
	TransferDeadline time.Duration
	CandidateBackoff time.Duration
	FailedRetryAfter time.Duration
	MaxCandidates    int
	Batch            int
	Logger           *slog.Logger
}

// Discoverer drives AlbumJobs through the pipeline, one transition per tick.
type Discoverer struct {
	p DiscovererParams
}

// NewDiscoverer constructs a Discoverer.
func NewDiscoverer(p DiscovererParams) *Discoverer { return &Discoverer{p: p} }

func (d *Discoverer) log() *slog.Logger {
	if d.p.Logger != nil {
		return d.p.Logger
	}
	return slog.Default()
}

// RunOnce performs one pipeline tick: sync wanted albums from Lidarr, then take
// each job one transition forward. Every stage is bounded and idempotent, so a
// crash mid-tick loses nothing.
func (d *Discoverer) RunOnce(ctx context.Context, now time.Time) error {
	if err := d.syncWanted(ctx, now); err != nil {
		return err
	}
	if err := d.startNewJobs(ctx, now); err != nil {
		return err
	}
	if err := d.advanceDownloading(ctx, now); err != nil {
		return err
	}
	return d.advanceImporting(ctx, now)
}

// syncWanted upserts every wanted Lidarr album as a DISCOVERED job (idempotent).
func (d *Discoverer) syncWanted(ctx context.Context, now time.Time) error {
	albums, err := d.p.Music.WantedMissing(ctx)
	if err != nil {
		return fmt.Errorf("wanted missing: %w", err)
	}
	for _, a := range albums {
		if _, err := d.p.Store.UpsertDiscoveredJob(ctx, a.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// startNewJobs searches and enqueues for DISCOVERED jobs and due COOLDOWN jobs.
func (d *Discoverer) startNewJobs(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateDiscovered, d.p.Batch)
	if err != nil {
		return err
	}
	due, err := d.p.Store.DueCooldownJobs(ctx, now, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range append(jobs, due...) {
		if err := d.startJob(ctx, job, now); err != nil {
			d.log().Error("start job failed", "album_job", job.ID, "err", err)
		}
	}
	return nil
}

// startJob searches for one album, picks the best untried candidate that passes
// the quality floor, write-ahead enqueues its files, and moves the job to
// DOWNLOADING. If no candidate remains it goes to COOLDOWN, or FAILED once the
// candidate budget is exhausted.
func (d *Discoverer) startJob(ctx context.Context, job core.AlbumJob, now time.Time) error {
	if job.CandidatesTried >= d.p.MaxCandidates {
		return d.p.Store.AdvanceJobState(ctx, job.ID, core.StateFailed, now)
	}
	// Which usernames have we already tried for this album?
	attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
	if err != nil {
		return err
	}
	tried := map[string]bool{}
	for _, a := range attempts {
		tried[a.Username] = true
	}

	album, err := d.albumFor(ctx, job)
	if err != nil {
		return err
	}
	results, err := d.p.Peers.Search(ctx, album.ArtistName+" "+album.Title, d.p.SearchTimeout)
	if err != nil {
		return err
	}
	candidates := d.p.Ranker.Rank(results)
	for _, cand := range candidates {
		if tried[cand.Username] {
			continue
		}
		// Prefer candidates offering at least the expected track count, but accept
		// any if none match (Lidarr is the final arbiter).
		attemptID, err := d.p.Store.CreateAttempt(ctx, job.ID, cand.Username, cand.Score, now)
		if err != nil {
			return err
		}
		deadline := now.Add(d.p.TransferDeadline)
		for _, f := range cand.Files {
			tid, err := d.p.Store.RecordEnqueueIntent(ctx, attemptID, cand.Username, f.Filename, deadline, now)
			if err != nil {
				return err
			}
			slskdID, err := d.p.Peers.Enqueue(ctx, cand.Username, f.Filename)
			if err != nil {
				d.log().Error("enqueue failed", "user", cand.Username, "file", f.Filename, "err", err)
				continue
			}
			_ = d.p.Store.AttachTransferID(ctx, tid, slskdID, now)
		}
		if err := d.p.Store.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
			return err
		}
		return d.p.Store.AdvanceJobState(ctx, job.ID, core.StateDownloading, now)
	}
	// No untried candidate available now: back off.
	return d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now)
}

// albumFor returns the Lidarr album matching a job (by lidarr_album_id).
func (d *Discoverer) albumFor(ctx context.Context, job core.AlbumJob) (lidarr.WantedAlbum, error) {
	albums, err := d.p.Music.WantedMissing(ctx)
	if err != nil {
		return lidarr.WantedAlbum{}, err
	}
	for _, a := range albums {
		if a.ID == job.LidarrAlbumID {
			return a, nil
		}
	}
	return lidarr.WantedAlbum{}, fmt.Errorf("album %d no longer wanted", job.LidarrAlbumID)
}

// advanceDownloading moves DOWNLOADING jobs to VERIFYING when all their active
// attempt's transfers are COMPLETED, or to COOLDOWN when any transfer failed.
func (d *Discoverer) advanceDownloading(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateDownloading, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
		if err != nil {
			return err
		}
		if len(attempts) == 0 {
			continue
		}
		active := attempts[len(attempts)-1] // most recent
		transfers, err := d.p.Store.TransfersForAttempt(ctx, active.ID)
		if err != nil {
			return err
		}
		allDone, anyFailed := len(transfers) > 0, false
		for _, t := range transfers {
			switch t.State {
			case core.TransferCompleted:
			case core.TransferErrored, core.TransferCancelled:
				anyFailed = true
				allDone = false
			default:
				allDone = false
			}
		}
		switch {
		case anyFailed:
			_ = d.p.Store.FailAttempt(ctx, active.ID, "transfer failed", now.Add(d.p.CandidateBackoff), now)
			_ = d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now)
		case allDone:
			_ = d.p.Store.AdvanceJobState(ctx, job.ID, core.StateVerifying, now)
		}
	}
	return nil
}

// advanceImporting handles VERIFYING and IMPORTING jobs: it asks Lidarr what it
// would import from the album folder; a clean candidate is imported (COMPLETED),
// a rejected one fails the candidate (COOLDOWN, retried with the next candidate).
func (d *Discoverer) advanceImporting(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateVerifying, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		active := attempts[len(attempts)-1]
		transfers, err := d.p.Store.TransfersForAttempt(ctx, active.ID)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(transfers))
		for _, t := range transfers {
			names = append(names, t.Filename)
		}
		folder := AlbumFolder(d.p.CompleteDir, names)
		items, err := d.p.Music.ManualImportCandidates(ctx, folder)
		if err != nil {
			return err
		}
		var importable []lidarr.ManualImportItem
		blocked := false
		for _, it := range items {
			if it.Importable {
				importable = append(importable, it)
			} else {
				blocked = true
			}
		}
		if len(importable) == 0 || blocked {
			// Rejected (quality/no match): fail this candidate and back off to retry
			// with the next candidate.
			d.log().Info("import rejected", "album_job", job.ID, "folder", folder)
			_ = d.p.Store.FailAttempt(ctx, active.ID, "import rejected", now.Add(d.p.CandidateBackoff), now)
			_ = d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now)
			continue
		}
		if err := d.p.Music.ExecuteManualImport(ctx, importable); err != nil {
			return err
		}
		_ = d.p.Store.SucceedAttempt(ctx, active.ID, now)
		_ = d.p.Store.AdvanceJobState(ctx, job.ID, core.StateCompleted, now)
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/engine/`
Expected: PASS (all engine tests, including the three discovery scenarios).

- [ ] **Step 6: Commit**

```bash
git add internal/engine/discovery.go internal/engine/discovery_test.go
git commit -m "feat(engine): Discoverer state machine (search, enqueue, verify, import)"
```

---

### Task 9: Engine loop wiring + main.go + config.example

**Files:**
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`
- Modify: `cmd/slusk/main.go`
- Modify: `config.example.toml`

**Interfaces:**
- Produces: `Params` gains `Discoverer *Discoverer`; `Run` starts a second ticker on `LidarrPoll` that calls `Discoverer.RunOnce`. Backward compatible: a nil `Discoverer` disables the discovery loop (existing engine test stays valid).

- [ ] **Step 1: Write the failing test**

Add to `internal/engine/engine_test.go`:
```go
func TestRunTicksDiscoveryLoop(t *testing.T) {
	store := &fakeStore{}
	peers := &fakePeers{}
	rec := NewReconciler(peers, store)

	music := &fakeMusic{}
	searcher := &fakeSearcher{}
	dp, _ := newDiscoParams(t, music, searcher)
	disco := NewDiscoverer(dp)

	eng := New(Params{
		Reconciler: rec,
		Discoverer: disco,
		StatusPoll: time.Hour,
		LidarrPoll: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if eng.DiscoverCount() == 0 {
		t.Error("expected at least one discovery tick")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestRunTicksDiscovery`
Expected: FAIL — `Params.Discoverer` / `DiscoverCount` undefined.

- [ ] **Step 3: Wire the second loop**

In `internal/engine/engine.go`: add `Discoverer *Discoverer` to `Params`, add a `discoverCount atomic.Int64` to `Engine`, add `DiscoverCount()`, and run a second ticker inside `Run`:
```go
// DiscoverCount reports how many discovery passes have run.
func (e *Engine) DiscoverCount() int64 { return e.discoverCount.Load() }
```
Replace the `Run` loop body so it selects on both tickers (guarding the discovery ticker when `Discoverer` is nil):
```go
func (e *Engine) Run(ctx context.Context) error {
	statusTicker := time.NewTicker(e.p.StatusPoll)
	defer statusTicker.Stop()

	var lidarrC <-chan time.Time
	if e.p.Discoverer != nil {
		lidarrTicker := time.NewTicker(e.p.LidarrPoll)
		defer lidarrTicker.Stop()
		lidarrC = lidarrTicker.C
		e.discoverOnce(ctx) // run once immediately on startup
	}
	e.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statusTicker.C:
			e.reconcileOnce(ctx)
		case <-lidarrC:
			e.discoverOnce(ctx)
		}
	}
}

func (e *Engine) discoverOnce(ctx context.Context) {
	if err := e.p.Discoverer.RunOnce(ctx, time.Now().UTC()); err != nil {
		if e.p.Logger != nil {
			e.p.Logger.Error("discovery failed", "err", err)
		}
	}
	e.discoverCount.Add(1)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/`
Expected: PASS (all).

- [ ] **Step 5: Wire main.go**

In `cmd/slusk/main.go`, construct the Lidarr client, matcher, and discoverer, and pass the discoverer into the engine. After the existing `reconciler` construction add:
```go
	lidarrClient := lidarr.New(cfg.Lidarr.URL, cfg.Lidarr.APIKey)
	scorer := matcher.NewWeighted(cfg.Engine.Weights, cfg.Engine.MinBitrate)
	discoverer := engine.NewDiscoverer(engine.DiscovererParams{
		Music: lidarrClient, Peers: peers, Store: st, Ranker: scorer,
		CompleteDir:      cfg.Paths.SlskdCompleteDir,
		SearchTimeout:    cfg.Engine.SearchTimeout.Duration,
		TransferDeadline: cfg.Engine.TransferDeadline.Duration,
		CandidateBackoff: cfg.Engine.CandidateBackoff.Duration,
		FailedRetryAfter: cfg.Engine.FailedRetryAfter.Duration,
		MaxCandidates:    cfg.Engine.MaxCandidatesPerAlbum,
		Batch:            cfg.Engine.MaxConcurrentSearches,
		Logger:           logger,
	})
```
and add `Discoverer: discoverer,` to the `engine.Params` literal. Add `"github.com/samuelenocsson/slusk/internal/lidarr"` and `"github.com/samuelenocsson/slusk/internal/matcher"` to the imports.

- [ ] **Step 6: Update config.example.toml**

Add to `config.example.toml` under `[engine]` the five new keys and a `[paths]` section, matching the fixture from Task 3 (with the real default path):
```toml
search_timeout = "30s"
min_bitrate = 192
max_concurrent_searches = 2
candidate_backoff = "10m"
failed_retry_after = "24h"
```
```toml
[paths]
slskd_complete_dir = "/music/slskd-downloads"
```

- [ ] **Step 7: Verify build + full suite + commit**

Run: `go build ./... && go test ./...`
Expected: builds; all packages pass.

```bash
git add internal/engine/ cmd/ config.example.toml
git commit -m "feat(engine): second discovery loop, wire Discoverer in main"
```

---

## Self-review notes

- **Spec coverage:** §2 control model (Tasks 8,9) · §3 async search (Task 1) · §4 quality floor + reliability + file-count hint (Tasks 4,8) · §5 manual-import rejection handling (Tasks 2,8) · §6 retry/backoff/fail (Tasks 5,8) · §7 path translation, now confirmed same-path (Task 7) · §8 store methods (Tasks 5,6) · §9 module changes (all) · v1 downstream note about global `UNIQUE(username,filename)` fixed by conflict-safe `RecordEnqueueIntent` (Task 6).
- **Placeholder scan:** no TBD/TODO in code steps; the one prose helper note (Task 8 Step 2, test helpers) is explicit about what to implement with named methods, not a hand-wave.
- **Type consistency:** `slskd.Result` extended fields (Task 1) used by matcher (Task 4) and discovery (Task 8); `lidarr.ManualImportItem`/`WantedAlbum.TrackCount` (Task 2) used by ports (Task 6) and discovery (Task 8); `DiscoveryStore`/`MusicSource`/`PeerSearcher`/`Ranker` (Task 6) consumed by `Discoverer` (Task 8); `Params.Discoverer`/`DiscoverCount` (Task 9) consistent across engine.go and its test.
- **Deferred still-deferred:** HTML UI, plugin scoring. Not touched here.
