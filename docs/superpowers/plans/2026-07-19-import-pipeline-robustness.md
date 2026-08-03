# Import Pipeline Robustness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the import pipeline tolerant of Soulseek's messiness: accept any valid album edition's track count (not just the canonical one), remove duplicate track files before Lidarr's import scan, and stop over-rejecting candidates on Lidarr's folder-level "Has unmatched tracks" stamp.

**Architecture:** Four changes along the existing `WANTED → SELECTING → DOWNLOADING → IMPORTING` pipeline. (1) A new Lidarr client call fetches per-release track counts; Discovery computes a `[min, max]` file-count band from them (replacing the ratio filter) and persists it on the job. (2) Importing's coverage gate compares against the persisted `MinTrackCount` instead of the canonical total. (3) A new tag-reading dedup pass runs at the top of `Importing.verify()`. (4) `verify()` classifies files as importable by `len(TrackIDs) > 0` instead of Lidarr's `Importable` flag.

**Tech Stack:** Go 1.26, `github.com/dhowden/tag` (new dependency, audio tag reading), embedded Postgres for store-backed tests.

**Spec:** `docs/superpowers/specs/2026-07-19-import-pipeline-robustness-design.md`

## Global Constraints

- Code and comments in English; conversation with Samuel in Swedish.
- Smallest reasonable change; match surrounding style exactly (this codebase has heavy doc comments on every exported and most unexported functions — keep that up).
- TDD: write the failing test first, watch it fail, then implement.
- Run tests with `go test ./internal/... ./cmd/...` from the repo root. Store-backed tests use embedded Postgres via `storetest.Run` — they work offline but take ~10-20 s to start the instance.
- **Breaking config change** (deliberate, approved): `pipeline.max_candidate_file_ratio` is removed entirely. Configs still carrying the key fail the unknown-key check at startup.
- Branch: work continues on `docs/import-pipeline-robustness-spec` (rename or branch off as appropriate when implementation starts, e.g. `feat/import-pipeline-robustness`).

---

### Task 1: Lidarr client — `AlbumReleases`

**Files:**
- Modify: `internal/lidarr/client.go` (append after `AlbumStatus`, ~line 233)
- Test: `internal/lidarr/client_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `type AlbumRelease struct { ID int64; TrackCount int; Monitored bool }` and `func (c *Client) AlbumReleases(ctx context.Context, albumID int64) ([]AlbumRelease, error)` — Task 3 adds this to the pipeline's `MusicSource` interface.

- [ ] **Step 1: Write the failing test**

Append to `internal/lidarr/client_test.go` (follow the file's existing httptest style, e.g. `TestAlbumStatus` at line 147):

```go
func TestAlbumReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/albumrelease" {
			t.Errorf("path = %q, want /api/v1/albumrelease", r.URL.Path)
		}
		if got := r.URL.Query().Get("albumId"); got != "42" {
			t.Errorf("albumId = %q, want 42", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "key" {
			t.Errorf("api key = %q, want key", got)
		}
		fmt.Fprint(w, `[
			{"id":1,"albumId":42,"trackCount":12,"monitored":true},
			{"id":2,"albumId":42,"trackCount":10,"monitored":false}
		]`)
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	rels, err := c.AlbumReleases(context.Background(), 42)
	if err != nil {
		t.Fatalf("AlbumReleases: %v", err)
	}
	want := []AlbumRelease{
		{ID: 1, TrackCount: 12, Monitored: true},
		{ID: 2, TrackCount: 10, Monitored: false},
	}
	if len(rels) != len(want) {
		t.Fatalf("got %d releases, want %d", len(rels), len(want))
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Errorf("release %d = %+v, want %+v", i, rels[i], want[i])
		}
	}
}
```

Add `"fmt"` to the test file's imports if not already there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lidarr/ -run TestAlbumReleases -v`
Expected: FAIL to compile with "undefined: AlbumRelease" / "c.AlbumReleases undefined".

- [ ] **Step 3: Implement**

Append to `internal/lidarr/client.go` after `AlbumStatus`:

```go
// AlbumRelease is one release (edition/pressing) of an album in Lidarr, with
// its own track count. Different releases of the same album legitimately have
// different track counts (bonus tracks, deluxe editions), and any of them is a
// valid import target since manual import runs with release switching enabled.
type AlbumRelease struct {
	ID         int64
	TrackCount int
	Monitored  bool
}

// AlbumReleases lists every release of an album, used by discovery to compute
// the valid track-count band [min, max] across all editions rather than
// filtering against the single canonical count.
func (c *Client) AlbumReleases(ctx context.Context, albumID int64) ([]AlbumRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/albumrelease?albumId=%d", c.baseURL, albumID), nil)
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
		return nil, fmt.Errorf("lidarr albumrelease: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID         int64 `json:"id"`
		TrackCount int   `json:"trackCount"`
		Monitored  bool  `json:"monitored"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]AlbumRelease, 0, len(raw))
	for _, r := range raw {
		out = append(out, AlbumRelease{ID: r.ID, TrackCount: r.TrackCount, Monitored: r.Monitored})
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/lidarr/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lidarr/client.go internal/lidarr/client_test.go
git commit -m "feat(lidarr): add AlbumReleases client call for per-release track counts"
```

---

### Task 2: Store — persist the track-count band on `album_jobs`

**Files:**
- Modify: `internal/store/schema.sql` (append at end)
- Modify: `internal/core/models.go` (AlbumJob struct, ~line 6-32)
- Modify: `internal/store/pipeline.go` (`jobSelect` line 13, `scanJobs` line 15-27; append `SetJobTrackBand`)
- Test: `internal/store/pipeline_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `core.AlbumJob.MinTrackCount int` / `core.AlbumJob.MaxTrackCount int` (populated by every query going through `jobSelect`, i.e. `RunnableJobsInState`), and `func (s *Store) SetJobTrackBand(ctx context.Context, jobID int64, minTracks, maxTracks int) error`. Task 3 calls `SetJobTrackBand`; Task 4 reads `job.MinTrackCount`.

- [ ] **Step 1: Write the failing test**

Append to `internal/store/pipeline_test.go` (same fixture pattern as `TestCountJobsInStates`):

```go
func TestSetJobTrackBand(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 400, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if job.MinTrackCount != 0 || job.MaxTrackCount != 0 {
		t.Fatalf("fresh job band = (%d,%d), want (0,0)", job.MinTrackCount, job.MaxTrackCount)
	}

	if err := s.SetJobTrackBand(ctx, job.ID, 10, 12); err != nil {
		t.Fatalf("SetJobTrackBand: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].MinTrackCount != 10 || jobs[0].MaxTrackCount != 12 {
		t.Errorf("band = (%d,%d), want (10,12)", jobs[0].MinTrackCount, jobs[0].MaxTrackCount)
	}
}
```

Note: `UpsertWantedJob` returns the job it scanned — check how it scans its row. If it does not go through `jobSelect` (it may use its own RETURNING clause), the fresh-band assertion still works because the Go zero value is 0; only extend its scan if it errors on column count. Do not widen unrelated queries.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSetJobTrackBand -v`
Expected: FAIL to compile with "s.SetJobTrackBand undefined" / "jobs[0].MinTrackCount undefined".

- [ ] **Step 3: Implement**

`internal/store/schema.sql`, append at the end:

```sql
-- Import-pipeline robustness (spec 2026-07-19): the album's valid track-count
-- band across all Lidarr releases (editions), cached by Discovery at search
-- time and read by Importing's coverage gate. (0,0) means unknown.
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS min_track_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS max_track_count BIGINT NOT NULL DEFAULT 0;
```

`internal/core/models.go`, add to `AlbumJob` after `FailedAt`:

```go
	// MinTrackCount/MaxTrackCount is the album's valid track-count band across
	// all Lidarr releases (editions), cached by Discovery at search time.
	// Importing's coverage gate accepts any candidate covering at least
	// MinTrackCount tracks. (0,0) means unknown — the gate then falls back to
	// the live AlbumStatus total.
	MinTrackCount int
	MaxTrackCount int
```

`internal/store/pipeline.go`, extend `jobSelect` (line 13) and `scanJobs`:

```go
const jobSelect = `SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id, retries, not_before, failed_at, min_track_count, max_track_count FROM album_jobs`
```

In `scanJobs`, extend the `Scan` call with `&j.MinTrackCount, &j.MaxTrackCount` at the end (matching the column order).

Append to `internal/store/pipeline.go`:

```go
// SetJobTrackBand caches the album's valid track-count band (min/max across
// all Lidarr releases) on the job, written by Discovery once per search. Like
// BackfillJobMetadataIfEmpty this is a metadata cache write, so updated_at is
// deliberately not bumped — the band must not reset fairness ordering or
// stuck-detection clocks.
func (s *Store) SetJobTrackBand(ctx context.Context, jobID int64, minTracks, maxTracks int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET min_track_count = $1, max_track_count = $2 WHERE id = $3`,
		minTracks, maxTracks, jobID)
	if err != nil {
		return fmt.Errorf("set job track band: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store/ -v -run 'TestSetJobTrackBand|TestRunnable'`
Expected: PASS. Then `go build ./...` — expect success (new AlbumJob fields are additive).

- [ ] **Step 5: Commit**

```bash
git add internal/store/schema.sql internal/core/models.go internal/store/pipeline.go internal/store/pipeline_test.go
git commit -m "feat(store): persist per-release track-count band on album_jobs"
```

---

### Task 3: Discovery — `[min, max]` band replaces the ratio filter

**Files:**
- Modify: `internal/pipeline/ports.go` (`MusicSource`, line 18-23)
- Modify: `internal/pipeline/discovery.go` (params line 41-65, `DiscoveryStore` line 27-39, filter block line 170-214, new `trackBand` helper)
- Modify: `internal/lidarr/client.go` (remove `WantedAlbum.TrackCount` + its parsing)
- Modify: `internal/config/config.go` (remove `MaxCandidateFileRatio` field line 81-86 and validation line 263-265)
- Modify: `cmd/slusk/main.go` (remove `MaxCandidateFileRatio:` line 80)
- Modify: `config.example.toml` (remove the key at line 42 with its 5-line comment block above it; reword the `[pipeline]` intro comment at line 15 that references `max_candidate_file_ratio` to reference `min_bitrate` instead)
- Modify: `internal/config/testdata/valid.toml`, `pipeline_invalid.toml`, `pipeline_unknown_key.toml`, `pipeline_overrides.toml` (each has `max_candidate_file_ratio = 2.0` at line 17 — remove; check `config_test.go` for assertions on the field and remove those too)
- Modify: `internal/pipeline/pipeline_shared_test.go` (`fakeMusic`)
- Test: `internal/pipeline/discovery_test.go`

**Interfaces:**
- Consumes: `lidarr.AlbumRelease` / `Client.AlbumReleases` (Task 1), `Store.SetJobTrackBand` + `AlbumJob.MinTrackCount/MaxTrackCount` (Task 2).
- Produces: `MusicSource` gains `AlbumReleases(ctx context.Context, albumID int64) ([]lidarr.AlbumRelease, error)`; `fakeMusic` gains `albumReleases []lidarr.AlbumRelease` and `albumReleasesErr error` (Tasks 4-5's tests may set them); jobs that have been searched carry a persisted band.

- [ ] **Step 1: Update the shared fake first (it's compile-blocking for the interface change)**

In `internal/pipeline/pipeline_shared_test.go`, add fields to `fakeMusic`:

```go
	// albumReleases/albumReleasesErr drive AlbumReleases, Discovery's source
	// for the album's valid track-count band.
	albumReleases    []lidarr.AlbumRelease
	albumReleasesErr error
```

and the method:

```go
func (f *fakeMusic) AlbumReleases(ctx context.Context, albumID int64) ([]lidarr.AlbumRelease, error) {
	if f.albumReleasesErr != nil {
		return nil, f.albumReleasesErr
	}
	return f.albumReleases, nil
}
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/pipeline/discovery_test.go`. Follow the file's existing fixture style (see `TestDiscoveryCachesRankedCandidates` at line 50 for how jobs, `fakeSearcher` results, and the Discovery constructor are built — reuse its helpers verbatim; the snippets below show the *assertion logic* to embed in that fixture style, with real ranked results giving one candidate with 9 files, one with 11, one with 30):

```go
// TestDiscoveryTrackBandFilter: releases 10 and 12 tracks → band [10,12].
// A 9-file candidate (below min) and a 30-file candidate (above max) are
// rejected; an 11-file candidate survives and is cached.
func TestDiscoveryTrackBandFilter(t *testing.T) {
	// fixture: job in WANTED, wanted snapshot present,
	// music := &fakeMusic{..., albumReleases: []lidarr.AlbumRelease{
	// 	{ID: 1, TrackCount: 12, Monitored: true},
	// 	{ID: 2, TrackCount: 10},
	// }}
	// searcher returns three users' results: 9, 11, and 30 files respectively.
	// After d.Tick:
	// - assert exactly the 11-file user's candidate was cached (via store.CandidatesForJob)
	// - assert the job advanced to SELECTING
	// - assert the persisted band: job re-read from store has MinTrackCount==10, MaxTrackCount==12
}

// TestDiscoveryTrackBandUnknownSkipsFilter: no releases with a positive
// track count → band (0,0) → no size filtering; all candidates cached.
func TestDiscoveryTrackBandUnknownSkipsFilter(t *testing.T) {
	// music := &fakeMusic{..., albumReleases: nil}
	// searcher returns a 3-file candidate for a job; assert it is cached.
}

// TestDiscoveryAlbumReleasesErrorLeavesJobUntouched: an AlbumReleases error
// aborts the pass without spending retry budget (same as the old AlbumStatus
// error handling).
func TestDiscoveryAlbumReleasesErrorLeavesJobUntouched(t *testing.T) {
	// music := &fakeMusic{..., albumReleasesErr: errors.New("boom")}
	// After d.Tick: job still WANTED, Retries still 0, no candidates cached.
}
```

Write these as real tests (full fixtures, no comments-as-code) — the comment bodies above define the required assertions.

Also update the existing tests mechanically:
- Remove `MaxCandidateFileRatio: 2.0` from the params literal at line 41.
- Every `fakeMusic{... albumTotal: N}` in `discovery_test.go` (e.g. line 55): replace with `albumReleases: []lidarr.AlbumRelease{{ID: 1, TrackCount: N, Monitored: true}}` so their file counts still pass the band. (`albumTotal` stays on the fake — importing tests still use it for `AlbumStatus`.)

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/pipeline/ -run TestDiscovery -v`
Expected: FAIL — the new tests fail (band filter not implemented; fake compiles once Step 1 is in, but Discovery still calls `AlbumStatus` and the ratio filter).

- [ ] **Step 4: Implement**

`internal/pipeline/ports.go` — add to `MusicSource`:

```go
	AlbumReleases(ctx context.Context, albumID int64) ([]lidarr.AlbumRelease, error)
```

`internal/pipeline/discovery.go`:

1. Delete the `MaxCandidateFileRatio float64` field and its doc comment from `DiscoveryParams` (lines 51-58).
2. Add to `DiscoveryStore` (after `AdvanceJobStateFrom`):

```go
	// SetJobTrackBand caches the album's valid track-count band on the job,
	// read later by Importing's coverage gate.
	SetJobTrackBand(ctx context.Context, jobID int64, minTracks, maxTracks int) error
```

3. Replace the `AlbumStatus` block (lines 170-182) and both filter branches inside the survivor loop (lines 194-212) with:

```go
	// Fetch the album's releases once per searchJob call to compute the valid
	// track-count band [min, max] across all editions — a candidate matching
	// any real edition's track count is viable, since manual import runs with
	// release switching enabled and Lidarr picks the matching edition itself.
	// A (0,0) band means Lidarr has no usable release data right now, so the
	// filter is skipped entirely rather than risk rejecting a legitimate
	// candidate on bad data. An error here is not counted against the job's
	// retry budget: it aborts this search pass early — log and return nil so
	// the job stays WANTED, untouched, and is retried on a later tick.
	releases, err := d.p.Music.AlbumReleases(ctx, job.LidarrAlbumID)
	if err != nil {
		d.log().Error("album releases failed", "album_job", job.ID, "err", err)
		return nil
	}
	minTracks, maxTracks := trackBand(releases)
	// Persisted (not just used inline) because Importing's coverage gate needs
	// MinTrackCount long after this search: a candidate covering the smallest
	// valid edition must not be rejected against the canonical (larger) count.
	if err := d.p.Store.SetJobTrackBand(ctx, job.ID, minTracks, maxTracks); err != nil {
		return err
	}
```

and inside the survivor loop, replacing both `total > 0` branches:

```go
		if maxTracks > 0 && len(cand.Files) > maxTracks {
			// More files than the largest known edition — almost certainly not
			// a single release (e.g. a whole discography in one flat folder).
			detail := fmt.Sprintf("candidate %s has more files than any known release (%d files, max %d), skipping", cand.Username, len(cand.Files), maxTracks)
			d.log().Info(detail, "album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "max", maxTracks)
			d.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
			continue
		}
		if minTracks > 0 && len(cand.Files) < minTracks {
			// Can't cover even the smallest edition — guaranteed to fail the
			// IMPORTING coverage gate after burning a full download cycle.
			detail := fmt.Sprintf("candidate %s has fewer files than the smallest release (%d files, min %d), skipping", cand.Username, len(cand.Files), minTracks)
			d.log().Info(detail, "album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "min", minTracks)
			d.recordEvent(ctx, job.ID, core.EventCandidateRejected, detail, now)
			continue
		}
```

4. Add the helper at the bottom of the file:

```go
// trackBand computes the valid track-count band across an album's releases:
// the smallest and largest positive track count. Releases with no track count
// (0) are ignored; (0, 0) means no usable release data at all.
func trackBand(releases []lidarr.AlbumRelease) (minTracks, maxTracks int) {
	for _, r := range releases {
		if r.TrackCount <= 0 {
			continue
		}
		if minTracks == 0 || r.TrackCount < minTracks {
			minTracks = r.TrackCount
		}
		if r.TrackCount > maxTracks {
			maxTracks = r.TrackCount
		}
	}
	return minTracks, maxTracks
}
```

`internal/lidarr/client.go` — remove `TrackCount int` from `WantedAlbum` (line 36), the `Statistics` block from `wantedMissingPage` (lines 54-56), and `TrackCount: r.Statistics.TrackCount,` from the mapping in `WantedMissing` (line 99). Fix any `client_test.go` assertion on the field.

Config removal:
- `internal/config/config.go`: delete the `MaxCandidateFileRatio` field + doc comment (lines 81-86) and its validation branch (lines 263-265).
- `cmd/slusk/main.go`: delete line 80 (`MaxCandidateFileRatio: cfg.Pipeline.MaxCandidateFileRatio,`).
- `config.example.toml`: delete `max_candidate_file_ratio = 2.0` and the 5-line comment above it; in the `[pipeline]` intro comment ("The matcher/transfer knobs below (through max_candidate_file_ratio and ..."), replace `max_candidate_file_ratio` with `min_bitrate`.
- The four testdata TOMLs: delete their `max_candidate_file_ratio = 2.0` line. Check `internal/config/config_test.go` for assertions on `MaxCandidateFileRatio` (e.g. in the valid-config or overrides test) and remove them, including any expected-problem string for `max_candidate_file_ratio` in the invalid-config test.

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/config/ ./internal/lidarr/ ./internal/pipeline/ -v`
Expected: all PASS (integration_test.go's `albumTotal: 1` fixtures still pass: their jobs have band (0,0) → filter skipped in discovery, and importing still uses `AlbumStatus` until Task 4's fallback keeps that behavior).

- [ ] **Step 6: Commit**

```bash
git add -A internal/ cmd/ config.example.toml
git commit -m "feat(discovery): filter candidates by per-release track-count band

Replaces the max_candidate_file_ratio heuristic with a [min, max] file-count
band computed from Lidarr's actual album releases, persisted on the job for
the importing coverage gate. Breaking config change: the
pipeline.max_candidate_file_ratio key is removed."
```

---

### Task 4: Importing — TrackID-based importable check + band-aware coverage gate

**Files:**
- Modify: `internal/pipeline/importing.go` (`verify`, lines 205-236)
- Test: `internal/pipeline/importing_test.go`

**Interfaces:**
- Consumes: `job.MinTrackCount` (Task 2; populated for searched jobs by Task 3), `lidarr.ManualImportItem.TrackIDs` (existing).
- Produces: no new symbols — behavior change only.

- [ ] **Step 1: Write the failing tests**

Append to `internal/pipeline/importing_test.go`, following the fixture style of `TestImportingVerifyRejectionFailsCandidateToSelecting` (line 89 — job seeded to IMPORTING with an active candidate and transfers; `fakeMusic.manualImportItems` drives verify):

```go
// TestImportingVerifyKeepsTrackMatchedFilesDespiteFolderRejection: Lidarr
// stamps "Has unmatched tracks" on every file in a non-bijective folder,
// including files that individually matched a track. Files with TrackIDs must
// be imported anyway; only genuinely unmatched files (no TrackIDs) are dropped.
func TestImportingVerifyKeepsTrackMatchedFilesDespiteFolderRejection(t *testing.T) {
	// fakeMusic.manualImportItems: two items WITH TrackIDs ({1},{2}) but each
	// carrying Rejections: []string{"Has unmatched tracks"} / Importable: false,
	// plus one item with TrackIDs: nil and the same rejection.
	// job.MinTrackCount is 2 (seed via s.SetJobTrackBand(ctx, job.ID, 2, 3)).
	// After m.Tick:
	// - assert ExecuteManualImport was called with exactly the two
	//   TrackID-carrying items (music.executedItems)
	// - assert candidate is still ACTIVE with ImportSubmittedAt set (verify
	//   submitted, moved to confirm phase) — NOT failed to SELECTING
}

// TestImportingVerifyAllUnmatchedStillFailsCandidate: when no file at all has
// a TrackID, the candidate fails to SELECTING exactly as before.
func TestImportingVerifyAllUnmatchedStillFailsCandidate(t *testing.T) {
	// manualImportItems: two items, both TrackIDs: nil, Rejections set.
	// Assert candidate FAILED with reason "import rejected", job in SELECTING.
}

// TestImportingCoverageUsesMinTrackCountBand: a candidate covering the
// smallest valid edition (2 tracks) passes even though the canonical
// AlbumStatus total is 3.
func TestImportingCoverageUsesMinTrackCountBand(t *testing.T) {
	// Seed s.SetJobTrackBand(ctx, job.ID, 2, 3); fakeMusic.albumTotal: 3.
	// manualImportItems: two clean items with TrackIDs {1}, {2}.
	// After m.Tick: assert import submitted (not "incomplete download").
}
```

Write these as real tests with full fixtures — the comments define required assertions. Also update `TestImportingVerifyRejectionFailsCandidateToSelecting` (line 89): its rejected items must now have `TrackIDs: nil` to still represent a fully-rejected candidate (check the fixture; if its items already lack TrackIDs, no change needed). `TestImportingIncompleteCoverageFailsCandidate` (line 117) keeps working via the band-(0,0)→`AlbumStatus` fallback.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pipeline/ -run TestImporting -v`
Expected: the three new tests FAIL (first: candidate wrongly failed to SELECTING; third: wrongly rejected as incomplete).

- [ ] **Step 3: Implement**

In `internal/pipeline/importing.go`, replace lines 205-236 (from `var importable` through the incomplete-coverage block) with:

```go
	// Classify by TrackIDs rather than Lidarr's per-file Importable flag:
	// Lidarr stamps folder-level rejections like "Has unmatched tracks" on
	// every file in a folder that isn't a perfect bijection against the
	// release — including files it did match to a track. A file with one or
	// more real track IDs was matched and is importable; only files with no
	// track ID at all are genuinely unmatched.
	var importable []lidarr.ManualImportItem
	var rejections []string
	for _, it := range items {
		if len(it.TrackIDs) > 0 {
			importable = append(importable, it)
		} else {
			rejections = append(rejections, it.Rejections...)
		}
	}
	if len(importable) == 0 {
		// Nothing matched at all — rejected like a failed download: other
		// candidates usually remain, so the next SELECTING tick tries one
		// immediately — no cooldown.
		rejectedDetail := fmt.Sprintf("import rejected (folder %s): %s", folder, strings.Join(rejections, "; "))
		m.log().Info(rejectedDetail, "album_job", job.ID, "folder", folder, "reasons", rejections)
		m.recordEvent(ctx, job.ID, core.EventImportRejected, rejectedDetail, now)
		return m.failCandidate(ctx, job, cand, names, "import rejected", now)
	}
	if len(rejections) > 0 {
		// Unmatched extras don't fail the candidate — the coverage gate below
		// decides completeness. They are left behind in the download folder
		// (cleanupCompletedFolder already leaves non-empty folders in place).
		m.log().Info("ignoring unmatched files in album folder",
			"album_job", job.ID, "folder", folder, "unmatched", len(items)-len(importable), "reasons", rejections)
	}
	// Completeness is judged against the smallest valid edition
	// (MinTrackCount, cached by Discovery from Lidarr's release list): with
	// release switching enabled, a candidate matching any real edition is a
	// full album. Fall back to the live canonical total only when the band was
	// never cached (0) — e.g. a job searched before the band existed.
	minRequired := job.MinTrackCount
	if minRequired == 0 {
		_, total, err := m.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
		if err != nil {
			m.log().Error("album status failed", "album_job", job.ID, "err", err)
			return m.escalateIfStuck(ctx, job, cand, names, "album status check failed", now)
		}
		minRequired = total
	}
	if coverage(importable) < minRequired {
		// A source that can't complete any valid edition is rejected outright
		// rather than partially imported, to keep the library free of half
		// albums. Other candidates usually remain, so use the no-cooldown
		// fail path.
		incompleteDetail := fmt.Sprintf("incomplete download, rejecting (folder %s, covered %d/%d)", folder, coverage(importable), minRequired)
		m.log().Info(incompleteDetail, "album_job", job.ID, "folder", folder,
			"covered", coverage(importable), "required", minRequired)
		m.recordEvent(ctx, job.ID, core.EventImportRejected, incompleteDetail, now)
		return m.failCandidate(ctx, job, cand, names, "incomplete download", now)
	}
```

Also update `verify`'s doc comment (lines 165-174): replace the sentence about "A candidate with any rejection fails outright" with the new rule (files with track IDs import despite folder-level rejections; a candidate with no matched file at all fails; completeness is judged against the smallest valid edition).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/pipeline/ -v`
Expected: all PASS, including integration tests (band-(0,0) fallback preserves their behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/importing.go internal/pipeline/importing_test.go
git commit -m "fix(importing): don't reject track-matched files on folder-level rejections

Lidarr stamps 'Has unmatched tracks' on every file in a non-bijective
folder, including correctly matched ones. Classify importability by
len(TrackIDs) > 0 instead, and judge completeness against the smallest
valid edition (MinTrackCount) rather than the canonical total."
```

---

### Task 5: Dedup — tag-based duplicate removal before the import scan

**Files:**
- Modify: `go.mod` / `go.sum` (`go get github.com/dhowden/tag@latest`)
- Create: `internal/pipeline/dedup.go`
- Create: `internal/pipeline/dedup_test.go`
- Modify: `internal/core/state.go` (add `EventDedup` to the `JobEventType` consts, line 67-77)
- Modify: `internal/pipeline/importing.go` (`verify`, right after `folder := AlbumFolder(...)` line 184)
- Test: `internal/pipeline/dedup_test.go`, `internal/pipeline/importing_test.go`

**Interfaces:**
- Consumes: `AlbumFolder` (existing, paths.go:62), `core.EventDedup` (added here).
- Produces: `func dedupAlbumFolder(log *slog.Logger, folder string) (removed []string, err error)`, pure helpers `dedupFiles`, `winner`, `normalizeTitle`, `trackKey`, type `dedupFile`, and the test seam `var readFileMeta func(path string, size int64) dedupFile`.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/dhowden/tag@latest && go mod tidy`
Expected: `go.mod` gains `github.com/dhowden/tag` in the main require block.

- [ ] **Step 2: Write the failing tests**

Create `internal/pipeline/dedup_test.go`:

```go
package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func df(path string, size int64, disc, track int, title string, lossless bool) dedupFile {
	return dedupFile{path: path, size: size, disc: disc, track: track, titleKey: normalizeTitle(title), lossless: lossless}
}

func loserPaths(files []dedupFile) []string {
	var out []string
	for _, f := range dedupFiles(files) {
		out = append(out, f.path)
	}
	slices.Sort(out)
	return out
}

func TestDedupFilesGroupsByDiscAndTrack(t *testing.T) {
	files := []dedupFile{
		df("a.flac", 30_000_000, 1, 1, "Song One", true),
		df("a.mp3", 8_000_000, 1, 1, "Song One", false),
		df("b.mp3", 9_000_000, 1, 2, "Song Two", false),
	}
	if got, want := loserPaths(files), []string{"a.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesLosslessBeatsLossyRegardlessOfSize(t *testing.T) {
	files := []dedupFile{
		df("small.flac", 5_000_000, 1, 1, "Song", true),
		df("big.mp3", 90_000_000, 1, 1, "Song", false),
	}
	if got, want := loserPaths(files), []string{"big.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesSizeBreaksTiesWithinFormatClass(t *testing.T) {
	files := []dedupFile{
		df("low.mp3", 6_000_000, 1, 1, "Song", false),
		df("high.mp3", 12_000_000, 1, 1, "Song", false),
	}
	if got, want := loserPaths(files), []string{"low.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesUntrackedFileJoinsNumberedGroupByTitle(t *testing.T) {
	files := []dedupFile{
		df("tagged.flac", 30_000_000, 1, 3, "My Song (feat. Someone)", true),
		df("untagged.mp3", 8_000_000, 0, 0, "my song", false),
	}
	if got, want := loserPaths(files), []string{"untagged.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesUnidentifiableFilesAreNeverRemoved(t *testing.T) {
	files := []dedupFile{
		df("mystery1.mp3", 8_000_000, 0, 0, "", false),
		df("mystery2.mp3", 8_000_000, 0, 0, "", false),
		df("song.flac", 30_000_000, 1, 1, "Song", true),
	}
	if got := loserPaths(files); len(got) != 0 {
		t.Errorf("losers = %v, want none", got)
	}
}

func TestDedupFilesDistinctTracksUntouched(t *testing.T) {
	files := []dedupFile{
		df("1.flac", 30_000_000, 1, 1, "One", true),
		df("2.flac", 31_000_000, 1, 2, "Two", true),
		df("d2-1.flac", 32_000_000, 2, 1, "One Reprise", true),
	}
	if got := loserPaths(files); len(got) != 0 {
		t.Errorf("losers = %v, want none", got)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Song One":                "songone",
		"  Song ONE!  ":           "songone",
		"Song (feat. X & Y)":      "song",
		"Song ft. Somebody":       "song",
		"01 - Song":               "01song",
	}
	for in, want := range cases {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDedupAlbumFolderRemovesLosersFromDisk exercises the folder-level entry
// point with readFileMeta stubbed (crafting real tagged audio fixtures is the
// tag library's job to parse, not ours to generate).
func TestDedupAlbumFolderRemovesLosersFromDisk(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.flac", "one.mp3", "two.mp3", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := readFileMeta
	t.Cleanup(func() { readFileMeta = orig })
	readFileMeta = func(path string, size int64) dedupFile {
		switch filepath.Base(path) {
		case "one.flac":
			return df(path, 30_000_000, 1, 1, "One", true)
		case "one.mp3":
			return df(path, 8_000_000, 1, 1, "One", false)
		default:
			return df(path, 9_000_000, 1, 2, "Two", false)
		}
	}

	removed, err := dedupAlbumFolder(slog.Default(), dir)
	if err != nil {
		t.Fatalf("dedupAlbumFolder: %v", err)
	}
	if len(removed) != 1 || filepath.Base(removed[0]) != "one.mp3" {
		t.Fatalf("removed = %v, want [one.mp3]", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "one.mp3")); !os.IsNotExist(err) {
		t.Error("one.mp3 still on disk")
	}
	for _, name := range []string{"one.flac", "two.mp3", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s unexpectedly gone: %v", name, err)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/pipeline/ -run 'TestDedup|TestNormalize' -v`
Expected: FAIL to compile ("undefined: dedupFile" etc.).

- [ ] **Step 4: Implement `internal/pipeline/dedup.go`**

```go
package pipeline

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/dhowden/tag"
)

// Soulseek users' shared folders are not guaranteed to be tidy: the same track
// can appear twice in one album folder (e.g. both a FLAC and an MP3 copy, or a
// stray duplicate). Lidarr's manual import treats such a folder as ambiguous,
// so before the import scan the folder is deduplicated down to one file per
// track, keyed on embedded tags: track number when present, normalized title
// as fallback. Per track, lossless always beats lossy; within the same format
// class the larger file (a bitrate proxy — tag headers don't carry bitrate)
// wins.

// audioExts are the file extensions considered audio files; anything else in
// the folder (covers, cue sheets, logs) is ignored entirely.
var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".ogg": true, ".opus": true,
	".wav": true, ".ape": true, ".wma": true, ".aac": true, ".aiff": true,
}

// losslessExts classifies by extension; readFileMeta upgrades m4a to lossless
// when the tag header identifies ALAC.
var losslessExts = map[string]bool{".flac": true, ".wav": true, ".ape": true, ".aiff": true}

// dedupFile is one audio file's identity for dedup grouping.
type dedupFile struct {
	path     string
	size     int64
	disc     int
	track    int
	titleKey string // normalizeTitle of the tag title; "" when untagged
	lossless bool
}

// readFileMeta parses one audio file's embedded tags into a dedupFile. A file
// whose tags can't be read still participates with whatever the extension
// tells us (it just can't be grouped, so it is never removed). Package-level
// var so tests can stub tag parsing instead of crafting real tagged audio
// fixtures.
var readFileMeta = func(path string, size int64) dedupFile {
	df := dedupFile{path: path, size: size, lossless: losslessExts[strings.ToLower(filepath.Ext(path))]}
	f, err := os.Open(path)
	if err != nil {
		return df
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return df
	}
	df.track, _ = m.Track()
	df.disc, _ = m.Disc()
	df.titleKey = normalizeTitle(m.Title())
	if ft := m.FileType(); ft == tag.FLAC || ft == tag.ALAC {
		df.lossless = true
	}
	return df
}

// dedupAlbumFolder removes duplicate track files from one album folder and
// returns the removed paths. Only the folder's direct entries are considered
// (slskd recreates a flat leaf folder per download). A remove failure is
// logged and skipped — a leftover duplicate degrades the import at worst,
// while blocking verify would strand the job.
func dedupAlbumFolder(log *slog.Logger, folder string) (removed []string, err error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	var files []dedupFile
	for _, e := range entries {
		if e.IsDir() || !audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, readFileMeta(filepath.Join(folder, e.Name()), info.Size()))
	}
	for _, loser := range dedupFiles(files) {
		if err := os.Remove(loser.path); err != nil {
			log.Warn("remove duplicate track file failed", "file", loser.path, "err", err)
			continue
		}
		removed = append(removed, loser.path)
	}
	return removed, nil
}

// dedupFiles groups files into same-track sets and returns the losers (every
// file except each group's winner). Grouping is track-number-first: files
// with a positive track number group on (disc, track); files without one join
// a numbered group whose title matches, or failing that group with other
// unnumbered files by title. A file with neither track number nor title can't
// be identified as a duplicate of anything and is never removed.
func dedupFiles(files []dedupFile) (losers []dedupFile) {
	var groups [][]dedupFile
	byNum := make(map[string]int)   // "disc/track" → groups index
	byTitle := make(map[string]int) // titleKey → groups index
	for _, f := range files {
		if f.track <= 0 {
			continue
		}
		k := fmt.Sprintf("%d/%d", f.disc, f.track)
		idx, ok := byNum[k]
		if !ok {
			groups = append(groups, nil)
			idx = len(groups) - 1
			byNum[k] = idx
		}
		groups[idx] = append(groups[idx], f)
		if f.titleKey != "" {
			if _, taken := byTitle[f.titleKey]; !taken {
				byTitle[f.titleKey] = idx
			}
		}
	}
	for _, f := range files {
		if f.track > 0 || f.titleKey == "" {
			continue
		}
		idx, ok := byTitle[f.titleKey]
		if !ok {
			groups = append(groups, nil)
			idx = len(groups) - 1
			byTitle[f.titleKey] = idx
		}
		groups[idx] = append(groups[idx], f)
	}
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		w := winner(g)
		for _, f := range g {
			if f.path != w.path {
				losers = append(losers, f)
			}
		}
	}
	return losers
}

// winner picks the file to keep from one same-track group: lossless beats
// lossy, then larger size (bitrate proxy), then lexicographically smallest
// path for determinism.
func winner(g []dedupFile) dedupFile {
	best := g[0]
	for _, f := range g[1:] {
		switch {
		case f.lossless != best.lossless:
			if f.lossless {
				best = f
			}
		case f.size != best.size:
			if f.size > best.size {
				best = f
			}
		case f.path < best.path:
			best = f
		}
	}
	return best
}

// featSuffix strips "(feat. X)" / "ft. X" style suffixes before comparing
// titles, so a tagged "Song (feat. X)" and a bare "Song" copy still match.
// The leading [\s([] requires a separator before feat/ft, so a title like
// "Shift work" (containing "ft" mid-word) is left alone.
var featSuffix = regexp.MustCompile(`(?i)[\s([](feat|ft|featuring)\.?\s.*$`)

// normalizeTitle reduces a tag title to a comparison key: feat-suffix
// stripped, lowercased, letters and digits only.
func normalizeTitle(s string) string {
	s = featSuffix.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
```

- [ ] **Step 5: Run the dedup tests**

Run: `go test ./internal/pipeline/ -run 'TestDedup|TestNormalize' -v`
Expected: all PASS.

- [ ] **Step 6: Commit the dedup unit**

```bash
git add go.mod go.sum internal/pipeline/dedup.go internal/pipeline/dedup_test.go
git commit -m "feat(pipeline): tag-based duplicate-track removal for album folders"
```

- [ ] **Step 7: Wire into verify (failing test first)**

Add `EventDedup` to `internal/core/state.go`'s const block (after `EventImportRejected`, line 75):

```go
	EventDedup             JobEventType = "dedup"
```

Append to `internal/pipeline/importing_test.go`:

```go
// TestImportingVerifyDedupsFolderBeforeImportScan: verify removes duplicate
// track files from the album folder before asking Lidarr what to import.
func TestImportingVerifyDedupsFolderBeforeImportScan(t *testing.T) {
	// Fixture like TestImportingHappyPathSubmitsThenConfirmsToDone (line 158),
	// but with a real album folder on disk under the test CompleteDir
	// containing "one.flac" and "one.mp3" (transfer filenames sharing one
	// remote leaf folder so AlbumFolder resolves to it), and readFileMeta
	// stubbed exactly as in TestDedupAlbumFolderRemovesLosersFromDisk.
	// After m.Tick (verify phase):
	// - assert one.mp3 is gone from disk and one.flac remains
	// - assert ManualImportCandidates was called (music.manualImportCalls)
	//   — i.e. dedup ran before the scan, and the scan still happened
}
```

Write as a real test with a full fixture. Run `go test ./internal/pipeline/ -run TestImportingVerifyDedups -v` — expected: FAIL (one.mp3 still on disk).

Then in `internal/pipeline/importing.go`, insert immediately after `folder := AlbumFolder(m.p.CompleteDir, names)` (line 184):

```go
	if folder != m.p.CompleteDir {
		// Best-effort dedup before Lidarr scans the folder: a messy share can
		// contain the same track twice (mixed formats, stray copies), which
		// makes Lidarr's matching ambiguous. Skipped when AlbumFolder fell
		// back to the download root itself — deduping there could remove
		// other albums' files. A dedup failure (e.g. folder already imported
		// and gone) must not block verify; the scan below copes either way.
		if removed, err := dedupAlbumFolder(m.log(), folder); err != nil {
			m.log().Warn("dedup album folder failed", "album_job", job.ID, "folder", folder, "err", err)
		} else if len(removed) > 0 {
			detail := fmt.Sprintf("removed %d duplicate track file(s) before import", len(removed))
			m.log().Info(detail, "album_job", job.ID, "folder", folder, "removed", removed)
			m.recordEvent(ctx, job.ID, core.EventDedup, detail, now)
		}
	}
```

- [ ] **Step 8: Run the full suite**

Run: `go test ./internal/... ./cmd/...`
Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/core/state.go internal/pipeline/importing.go internal/pipeline/importing_test.go
git commit -m "feat(importing): dedup album folder before Lidarr's import scan"
```

---

### Task 6: Final verification and docs

**Files:**
- Modify: `docs/superpowers/specs/2026-07-19-import-pipeline-robustness-design.md` (status line only)

- [ ] **Step 1: Full build + test sweep**

Run: `go build ./... && go vet ./... && go test ./internal/... ./cmd/...`
Expected: clean build, no vet findings on changed packages, all tests PASS.

- [ ] **Step 2: Grep for leftovers**

Run: `grep -rn "MaxCandidateFileRatio\|max_candidate_file_ratio\|Statistics.TrackCount" --include="*.go" --include="*.toml" .`
Expected: no hits (outside docs/).

- [ ] **Step 3: Update spec status and commit**

Change the spec's `**Status:**` line to `Implemented`.

```bash
git add docs/superpowers/specs/2026-07-19-import-pipeline-robustness-design.md
git commit -m "docs: mark import-pipeline-robustness spec implemented"
```
