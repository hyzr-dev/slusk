# slusk Dashboard (Overview + Queue) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a real, data-backed web dashboard (Overview + Queue views) to slusk, served from the existing `observ.Server`, with a Cancel action wired to the real engine.

**Architecture:** Extend `internal/observ` (currently `/metrics` + `/status`) with `GET /`, `GET /dashboard.js`, `GET /api/jobs`, and `POST /api/jobs/{id}/cancel`. `observ` stays a leaf package — it takes new `JobsFunc`/`CancelFunc` closures from `main.go`, which has access to `store` and the `slskd` client. A new `internal/store/dashboard.go` adds a `ListJobsWithTransfer` join query and a `core.JobView` read-only projection. `album_jobs` gets two new cached columns (`title`, `artist_name`) filled in by the discoverer.

**Tech Stack:** Go `html/template` (embedded via `//go:embed`), vanilla JS polling `/api/jobs` every 3s, no new Go dependencies, no JS build step.

## Global Constraints

- No new Go module dependencies (project has zero extra deps beyond `modernc.org/sqlite` and `prometheus/client_golang`) — everything ships with Go stdlib `net/http`, `html/template`, `embed`.
- `internal/observ` must not import `internal/store` or `internal/engine` directly (existing package doc comment: "does not depend back on engine or store") — all data access goes through function-type parameters, matching the existing `StatusFunc` pattern.
- Schema changes must be additive and idempotent: `internal/store.Open` re-applies `schema.sql` on every start (`db.Exec(schemaSQL)`), and existing tables are created with `CREATE TABLE IF NOT EXISTS`, so adding columns requires a separate idempotent `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` (supported since SQLite 3.35.0; this project vendors `modernc.org/sqlite v1.53.0`, which bundles a recent SQLite).
- Do not change `UpsertDiscoveredJob`'s existing signature — it has 14+ call sites across `internal/store/*_test.go`, `internal/engine/discovery_test.go`, and `internal/engine/discovery.go`. Add a separate `UpdateJobMetadata` method instead (smallest reasonable change).
- Orphaned transfers (slskd transfers with no matching `album_jobs` row) are NOT representable per-row in this plan — the reconciler only counts them (`stats.Unknown`, `internal/engine/reconciler.go:144`). `/api/jobs` returns job-backed rows only.
- Follow existing code style: doc comments on every exported type/func explaining *why*, table-driven or scenario-named tests matching the patterns in `internal/store/jobs_test.go` and `internal/observ/observ_test.go`.

---

## File Structure

- Modify: `internal/store/schema.sql` — add `title`/`artist_name` columns to `album_jobs`
- Modify: `internal/core/models.go` — add `Title`, `ArtistName` fields to `AlbumJob`; add new `JobView` type
- Modify: `internal/store/jobs.go` — update `jobSelect`-equivalent scan for the new columns (actually lives in `pipeline.go`'s `jobSelect`/`scanJobs`); add `UpdateJobMetadata`
- Modify: `internal/store/pipeline.go` — update `jobSelect` constant and `scanJobs` to read the two new columns
- Create: `internal/store/dashboard.go` — `ListJobsWithTransfer`, `JobWithTransfer` (single-job lookup for cancel)
- Create: `internal/store/dashboard_test.go` — tests for both new store methods
- Modify: `internal/engine/ports.go` — add `UpdateJobMetadata` to `DiscoveryStore` interface
- Modify: `internal/engine/discovery.go` — call `UpdateJobMetadata` from `syncWanted`
- Modify: `internal/engine/discovery_test.go` — add `UpdateJobMetadata` to the fake store, assert it's called
- Modify: `internal/observ/observ.go` — add `JobsFunc`, `CancelFunc` types; extend `NewServer` signature; add the four new routes; add JSON DTOs for `/api/jobs`
- Create: `internal/observ/web/dashboard.html` — the page template (embedded)
- Create: `internal/observ/web/dashboard.js` — polling/rendering script (embedded)
- Create: `internal/observ/web.go` — `//go:embed web` + template parsing + handler funcs for `/`, `/dashboard.js`
- Modify: `internal/observ/observ_test.go` — update `NewServer` calls for the new signature; add tests for `/api/jobs` and `/api/jobs/{id}/cancel`
- Modify: `cmd/slusk/main.go` — build `jobsFn`/`cancelFn` closures over `st` and `peers`, pass to `observ.NewServer`

---

## Task 1: Cache title/artist_name on album_jobs

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/core/models.go`
- Modify: `internal/store/pipeline.go` (`jobSelect` const, `scanJobs` func)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `core.AlbumJob.Title string`, `core.AlbumJob.ArtistName string` — every later task reading a job gets these populated (empty string if never set).

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestSchemaHasTitleAndArtistColumns(t *testing.T) {
	s := newTestStore(t)

	cols := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(album_jobs)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		cols[name] = true
	}
	if !cols["title"] {
		t.Error("album_jobs missing title column")
	}
	if !cols["artist_name"] {
		t.Error("album_jobs missing artist_name column")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/... -run TestSchemaHasTitleAndArtistColumns -v`
Expected: FAIL — `album_jobs missing title column` and `album_jobs missing artist_name column`

- [ ] **Step 3: Add the columns to schema.sql**

Edit `internal/store/schema.sql`, changing the `album_jobs` table definition and adding idempotent `ALTER TABLE` statements right after it (so both fresh databases via `CREATE TABLE` and existing databases via `ALTER TABLE` end up with the columns):

```sql
CREATE TABLE IF NOT EXISTS album_jobs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    lidarr_album_id  INTEGER NOT NULL,
    state            TEXT NOT NULL,
    candidates_tried INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  DATETIME,
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    artist_name      TEXT NOT NULL DEFAULT '',
    UNIQUE(lidarr_album_id)
);

ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS artist_name TEXT NOT NULL DEFAULT '';
```

(The `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` lines are no-ops on a freshly created table since the columns already exist from `CREATE TABLE`, and are the only way an already-existing database — created before this change — picks up the new columns on next `store.Open`.)

- [ ] **Step 4: Add the fields to core.AlbumJob**

Edit `internal/core/models.go`:

```go
// AlbumJob is the unit of user-visible work: one wanted album from Lidarr.
type AlbumJob struct {
	ID              int64
	LidarrAlbumID   int64
	State           AlbumJobState
	CandidatesTried int
	NextAttemptAt   *time.Time // set while in COOLDOWN
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Title           string // cached from Lidarr at discovery time, for display only
	ArtistName      string // cached from Lidarr at discovery time, for display only
}
```

- [ ] **Step 5: Update jobSelect and scanJobs to read the new columns**

Edit `internal/store/pipeline.go`:

```go
const jobSelect = `SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name FROM album_jobs`

func scanJobs(rows *sql.Rows) ([]core.AlbumJob, error) {
	var out []core.AlbumJob
	for rows.Next() {
		var j core.AlbumJob
		var state string
		if err := rows.Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.Title, &j.ArtistName); err != nil {
			return nil, err
		}
		j.State = core.AlbumJobState(state)
		out = append(out, j)
	}
	return out, rows.Err()
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/... -run TestSchemaHasTitleAndArtistColumns -v`
Expected: PASS

- [ ] **Step 7: Run the full store test suite to check nothing else broke**

Run: `go test ./internal/store/... -v`
Expected: PASS (all tests, including existing `TestOpenAppliesSchema`, `TestUpsertDiscoveredJobIsIdempotent`, etc.)

- [ ] **Step 8: Commit**

```bash
git add internal/store/schema.sql internal/core/models.go internal/store/pipeline.go internal/store/store_test.go
git commit -m "feat(store): cache title/artist_name on album_jobs"
```

---

## Task 2: UpdateJobMetadata store method

**Files:**
- Modify: `internal/store/jobs.go`
- Test: `internal/store/jobs_test.go`

**Interfaces:**
- Consumes: nothing new (uses `s.db`, `core.AlbumJob` from Task 1)
- Produces: `func (s *Store) UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName string, now time.Time) error` — Task 3 calls this.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/jobs_test.go`:

```go
func TestUpdateJobMetadataSetsTitleAndArtist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 42, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if job.Title != "" || job.ArtistName != "" {
		t.Fatalf("expected empty title/artist before UpdateJobMetadata, got %q / %q", job.Title, job.ArtistName)
	}

	if err := s.UpdateJobMetadata(ctx, job.ID, "Untrue", "Burial", now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	jobs, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Title != "Untrue" || jobs[0].ArtistName != "Burial" {
		t.Errorf("title/artist = %q / %q, want Untrue / Burial", jobs[0].Title, jobs[0].ArtistName)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/... -run TestUpdateJobMetadataSetsTitleAndArtist -v`
Expected: FAIL with `s.UpdateJobMetadata undefined`

- [ ] **Step 3: Implement UpdateJobMetadata**

Add to `internal/store/jobs.go` (after `UpsertDiscoveredJob`):

```go
// UpdateJobMetadata refreshes the cached title/artist_name for a job. It is
// called every discovery pass so display metadata stays current even if
// Lidarr renames an album/artist after the job was first discovered.
func (s *Store) UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET title = ?, artist_name = ?, updated_at = ? WHERE id = ?`,
		title, artistName, now, jobID)
	if err != nil {
		return fmt.Errorf("update job metadata: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/... -run TestUpdateJobMetadataSetsTitleAndArtist -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/jobs.go internal/store/jobs_test.go
git commit -m "feat(store): add UpdateJobMetadata"
```

---

## Task 3: Wire discoverer to cache title/artist on every sync

**Files:**
- Modify: `internal/engine/ports.go`
- Modify: `internal/engine/discovery.go`
- Modify: `internal/engine/discovery_test.go`

**Interfaces:**
- Consumes: `store.UpdateJobMetadata` (Task 2), `lidarr.WantedAlbum{Title, ArtistName}` (already exists at `internal/lidarr/client.go:30-31`)
- Produces: every `DISCOVERED`/synced job has current `Title`/`ArtistName` populated after a discovery pass — Task 4's `ListJobsWithTransfer` relies on this for display.

- [ ] **Step 1: Add UpdateJobMetadata to the DiscoveryStore interface**

Edit `internal/engine/ports.go`, in the `DiscoveryStore` interface (after the `UpsertDiscoveredJob` line):

```go
type DiscoveryStore interface {
	UpsertDiscoveredJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error)
	UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName string, now time.Time) error
	JobsInState(ctx context.Context, state core.AlbumJobState, limit int) ([]core.AlbumJob, error)
	// ... (rest unchanged)
```

- [ ] **Step 2: Write the failing test**

Add to `internal/engine/discovery_test.go`. First locate the fake store type used across that file (it implements `DiscoveryStore`) — search for `func (b *` or `type .*Store` near the top of the file to find its name, then add an `UpdateJobMetadata` method and a way to assert it was called. Add this test:

```go
func TestSyncWantedCachesTitleAndArtist(t *testing.T) {
	b := newFakeStore() // use whatever constructor the existing fake store in this file uses
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	music := &fakeMusicSource{albums: []lidarr.WantedAlbum{
		{ID: 900, Title: "Dummy", ArtistName: "Portishead", TrackCount: 11},
	}}
	d := NewDiscoverer(DiscovererParams{Music: music, Store: b, Logger: testLogger()})

	if err := d.syncWanted(ctx, music.albums, now); err != nil {
		t.Fatalf("syncWanted: %v", err)
	}

	jobs, err := b.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Title != "Dummy" || jobs[0].ArtistName != "Portishead" {
		t.Errorf("title/artist = %q / %q, want Dummy / Portishead", jobs[0].Title, jobs[0].ArtistName)
	}
}
```

Before writing this step for real, read `internal/engine/discovery_test.go` in full to find: the actual fake store constructor name, whether a `fakeMusicSource` type already exists (reuse it — don't redefine), and whether a `testLogger()` helper exists (if not, pass `nil` — `Discoverer.log()` falls back to `slog.Default()`). Adjust the test above to match what's actually there rather than inventing new helper names.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/engine/... -run TestSyncWantedCachesTitleAndArtist -v`
Expected: FAIL (title/artist still empty, since `syncWanted` doesn't call `UpdateJobMetadata` yet)

- [ ] **Step 4: Implement the call in syncWanted**

Edit `internal/engine/discovery.go`, the `syncWanted` function (currently `discovery.go:79-86`):

```go
// syncWanted upserts every wanted Lidarr album as a DISCOVERED job (idempotent),
// and refreshes the job's cached title/artist metadata every pass.
func (d *Discoverer) syncWanted(ctx context.Context, albums []lidarr.WantedAlbum, now time.Time) error {
	for _, a := range albums {
		job, err := d.p.Store.UpsertDiscoveredJob(ctx, a.ID, now)
		if err != nil {
			return err
		}
		if err := d.p.Store.UpdateJobMetadata(ctx, job.ID, a.Title, a.ArtistName, now); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: Add UpdateJobMetadata to the fake store in discovery_test.go**

The fake store type in `internal/engine/discovery_test.go` must satisfy the now-larger `DiscoveryStore` interface or the package won't compile. Find its `UpsertDiscoveredJob` method and add a sibling method that mutates the same in-memory job record's `Title`/`ArtistName` fields, e.g.:

```go
func (b *fakeStore) UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName string, now time.Time) error {
	for i := range b.jobs {
		if b.jobs[i].ID == jobID {
			b.jobs[i].Title = title
			b.jobs[i].ArtistName = artistName
			b.jobs[i].UpdatedAt = now
			return nil
		}
	}
	return fmt.Errorf("job %d not found", jobID)
}
```

(Match this to the fake store's actual field names — read its `UpsertDiscoveredJob` implementation first to see how it stores jobs, e.g. `b.jobs map[int64]core.AlbumJob` vs a slice, and adjust the loop/lookup accordingly.)

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/engine/... -run TestSyncWantedCachesTitleAndArtist -v`
Expected: PASS

- [ ] **Step 7: Run the full engine test suite**

Run: `go test ./internal/engine/... -v`
Expected: PASS (all tests — confirms the interface change didn't break any other fake-store user in the package)

- [ ] **Step 8: Commit**

```bash
git add internal/engine/ports.go internal/engine/discovery.go internal/engine/discovery_test.go
git commit -m "feat(engine): cache Lidarr title/artist on every discovery sync"
```

---

## Task 4: ListJobsWithTransfer and JobWithTransfer store methods

**Files:**
- Create: `internal/store/dashboard.go`
- Create: `internal/store/dashboard_test.go`
- Modify: `internal/core/models.go`

**Interfaces:**
- Consumes: `album_jobs`, `candidate_attempts`, `transfers` tables (schema from Task 1); `core.AlbumJob`, `core.Transfer`, `core.AlbumJobState`, `core.TransferState`.
- Produces:
  - `type core.JobView struct { Job AlbumJob; Transfer *Transfer; Peer string }`
  - `func (s *Store) ListJobsWithTransfer(ctx context.Context) ([]core.JobView, error)`
  - `func (s *Store) JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error)`
  These are consumed by Task 6 (`/api/jobs`) and Task 7 (`/api/jobs/{id}/cancel`).

- [ ] **Step 1: Add JobView to core**

Edit `internal/core/models.go`, add at the end:

```go
// JobView is a read-only projection joining an AlbumJob with its most recent
// transfer, for display purposes only (e.g. the dashboard). It is never
// written back to the store.
type JobView struct {
	Job      AlbumJob
	Transfer *Transfer // nil if the job has no attempt/transfer yet
	Peer     string    // convenience copy of Transfer.Username; "" if Transfer is nil
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/store/dashboard_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

func TestListJobsWithTransferIncludesJobsWithoutAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if err := s.UpdateJobMetadata(ctx, job.ID, "Rounds", "Four Tet", now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Job.Title != "Rounds" || v.Job.ArtistName != "Four Tet" {
		t.Errorf("title/artist = %q / %q, want Rounds / Four Tet", v.Job.Title, v.Job.ArtistName)
	}
	if v.Transfer != nil {
		t.Errorf("expected nil Transfer for a job with no attempt, got %+v", v.Transfer)
	}
	if v.Peer != "" {
		t.Errorf("expected empty Peer, got %q", v.Peer)
	}
}

func TestListJobsWithTransferJoinsLatestTransfer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 2, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Dummy", "Portishead", now)

	// First (older) attempt/transfer.
	a1, err := s.CreateAttempt(ctx, job.ID, "peer_one", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt a1: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a1, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent a1: %v", err)
	}

	// Second (newer) attempt/transfer — this is the one ListJobsWithTransfer must surface.
	later := now.Add(time.Minute)
	a2, err := s.CreateAttempt(ctx, job.ID, "peer_two", 2.0, later)
	if err != nil {
		t.Fatalf("CreateAttempt a2: %v", err)
	}
	tid2, err := s.RecordEnqueueIntent(ctx, a2, "peer_two", "f2.flac", later.Add(time.Hour), later)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent a2: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid2, core.TransferInProgress, 512, 1024, later); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Transfer == nil {
		t.Fatalf("expected non-nil Transfer")
	}
	if v.Peer != "peer_two" {
		t.Errorf("Peer = %q, want peer_two (the newer attempt)", v.Peer)
	}
	if v.Transfer.State != core.TransferInProgress || v.Transfer.BytesDone != 512 {
		t.Errorf("Transfer = %+v, want state IN_PROGRESS bytesDone 512", v.Transfer)
	}
}

func TestListJobsWithTransferExcludesCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 3, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("expected 0 views (cancelled job excluded), got %d", len(views))
	}
}

func TestJobWithTransferNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, found, err := s.JobWithTransfer(ctx, 99999)
	if err != nil {
		t.Fatalf("JobWithTransfer: %v", err)
	}
	if found {
		t.Error("expected found=false for nonexistent job id")
	}
}

func TestJobWithTransferFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 4, now)
	a1, _ := s.CreateAttempt(ctx, job.ID, "solo_peer", 1.0, now)
	if _, err := s.RecordEnqueueIntent(ctx, a1, "solo_peer", "solo.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}

	v, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobWithTransfer: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if v.Peer != "solo_peer" {
		t.Errorf("Peer = %q, want solo_peer", v.Peer)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/store/... -run 'TestListJobsWithTransfer|TestJobWithTransfer' -v`
Expected: FAIL — `s.ListJobsWithTransfer undefined`, `s.JobWithTransfer undefined`

- [ ] **Step 4: Implement dashboard.go**

Create `internal/store/dashboard.go`:

```go
// Package store: dashboard.go holds read-only projections used by the web
// dashboard (internal/observ). Nothing here mutates state.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/samuelenocsson/slusk/internal/core"
)

// jobViewSelect joins each non-cancelled album_job with its most recent
// candidate_attempts row (by created_at) and that attempt's transfer, if any.
// A job with no attempts yet still appears, with NULL attempt/transfer columns.
const jobViewSelect = `
	SELECT
		j.id, j.lidarr_album_id, j.state, j.candidates_tried, j.next_attempt_at, j.created_at, j.updated_at, j.title, j.artist_name,
		t.id, t.attempt_id, t.slskd_id, t.username, t.filename, t.state, t.bytes_done, t.bytes_total, t.deadline, t.last_progress_at, t.updated_at
	FROM album_jobs j
	LEFT JOIN candidate_attempts a ON a.id = (
		SELECT id FROM candidate_attempts WHERE album_job_id = j.id ORDER BY created_at DESC LIMIT 1
	)
	LEFT JOIN transfers t ON t.attempt_id = a.id
	WHERE j.state != ?`

func scanJobView(r rowScanner) (core.JobView, error) {
	var v core.JobView
	var jState string
	var tID sql.NullInt64
	var tAttemptID sql.NullInt64
	var tSlskdID, tUsername, tFilename, tState sql.NullString
	var tBytesDone, tBytesTotal sql.NullInt64
	var tDeadline, tLastProgressAt, tUpdatedAt sql.NullTime

	err := r.Scan(
		&v.Job.ID, &v.Job.LidarrAlbumID, &jState, &v.Job.CandidatesTried, &v.Job.NextAttemptAt, &v.Job.CreatedAt, &v.Job.UpdatedAt, &v.Job.Title, &v.Job.ArtistName,
		&tID, &tAttemptID, &tSlskdID, &tUsername, &tFilename, &tState, &tBytesDone, &tBytesTotal, &tDeadline, &tLastProgressAt, &tUpdatedAt,
	)
	if err != nil {
		return core.JobView{}, err
	}
	v.Job.State = core.AlbumJobState(jState)

	if tID.Valid {
		tr := &core.Transfer{
			ID:         tID.Int64,
			AttemptID:  tAttemptID.Int64,
			SlskdID:    tSlskdID.String,
			Username:   tUsername.String,
			Filename:   tFilename.String,
			State:      core.TransferState(tState.String),
			BytesDone:  tBytesDone.Int64,
			BytesTotal: tBytesTotal.Int64,
			Deadline:   tDeadline.Time,
			UpdatedAt:  tUpdatedAt.Time,
		}
		if tLastProgressAt.Valid {
			lp := tLastProgressAt.Time
			tr.LastProgressAt = &lp
		}
		v.Transfer = tr
		v.Peer = tUsername.String
	}
	return v, nil
}

// ListJobsWithTransfer returns every non-cancelled album job joined with its
// most recent transfer, newest job first. Used by the dashboard's Queue view.
func (s *Store) ListJobsWithTransfer(ctx context.Context) ([]core.JobView, error) {
	rows, err := s.db.QueryContext(ctx, jobViewSelect+` ORDER BY j.updated_at DESC`, string(core.StateCancelled))
	if err != nil {
		return nil, fmt.Errorf("list jobs with transfer: %w", err)
	}
	defer rows.Close()

	var out []core.JobView
	for rows.Next() {
		v, err := scanJobView(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job view: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// JobWithTransfer looks up a single job (regardless of state) with its most
// recent transfer, for the cancel endpoint. found is false if no job has
// that id.
func (s *Store) JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT
			j.id, j.lidarr_album_id, j.state, j.candidates_tried, j.next_attempt_at, j.created_at, j.updated_at, j.title, j.artist_name,
			t.id, t.attempt_id, t.slskd_id, t.username, t.filename, t.state, t.bytes_done, t.bytes_total, t.deadline, t.last_progress_at, t.updated_at
		FROM album_jobs j
		LEFT JOIN candidate_attempts a ON a.id = (
			SELECT id FROM candidate_attempts WHERE album_job_id = j.id ORDER BY created_at DESC LIMIT 1
		)
		LEFT JOIN transfers t ON t.attempt_id = a.id
		WHERE j.id = ?`, jobID)

	v, err := scanJobView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.JobView{}, false, nil
	}
	if err != nil {
		return core.JobView{}, false, fmt.Errorf("job with transfer: %w", err)
	}
	return v, true, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/... -run 'TestListJobsWithTransfer|TestJobWithTransfer' -v`
Expected: PASS (all 5 tests)

- [ ] **Step 6: Run the full store test suite**

Run: `go test ./internal/store/... -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/core/models.go internal/store/dashboard.go internal/store/dashboard_test.go
git commit -m "feat(store): add ListJobsWithTransfer and JobWithTransfer projections"
```

---

## Task 5: Job-state-to-dashboard-status mapping helper

**Files:**
- Create: `internal/observ/status.go`
- Create: `internal/observ/status_test.go`

**Interfaces:**
- Consumes: `core.JobView` (Task 4), `core.AlbumJobState`, `core.TransferState`, `core.StateFailed` (need to check for "stalled" — see below).
- Produces: `func dashboardStatus(v core.JobView) string` returning one of `"queued"`, `"active"`, `"stalled"`, `"done"`, `"failed"` — consumed by Task 6's JSON mapping.

- [ ] **Step 1: Write the failing tests**

Create `internal/observ/status_test.go`:

```go
package observ

import (
	"testing"

	"github.com/samuelenocsson/slusk/internal/core"
)

func TestDashboardStatus(t *testing.T) {
	cases := []struct {
		name string
		v    core.JobView
		want string
	}{
		{
			name: "no transfer yet is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateDiscovered}},
			want: "queued",
		},
		{
			name: "searching with no transfer is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateSearching}},
			want: "queued",
		},
		{
			name: "transfer in progress is active",
			v: core.JobView{
				Job:      core.AlbumJob{State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferInProgress},
			},
			want: "active",
		},
		{
			name: "transfer stalled is stalled",
			v: core.JobView{
				Job:      core.AlbumJob{State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferStalled},
			},
			want: "stalled",
		},
		{
			name: "job completed is done",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateCompleted}},
			want: "done",
		},
		{
			name: "job failed is failed",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateFailed}},
			want: "failed",
		},
		{
			name: "job in cooldown is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateCooldown}},
			want: "queued",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dashboardStatus(c.v)
			if got != c.want {
				t.Errorf("dashboardStatus() = %q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/observ/... -run TestDashboardStatus -v`
Expected: FAIL — `undefined: dashboardStatus`

- [ ] **Step 3: Implement dashboardStatus**

Create `internal/observ/status.go`:

```go
// Package observ: status.go maps internal job/transfer states to the small
// display vocabulary the dashboard's Queue view uses (queued/active/
// stalled/done/failed), decoupling the UI from the engine's richer state
// machine (internal/core.AlbumJobState has 10 states; the dashboard needs 5).
package observ

import "github.com/samuelenocsson/slusk/internal/core"

// dashboardStatus derives the dashboard's coarse status label for a job view.
func dashboardStatus(v core.JobView) string {
	switch v.Job.State {
	case core.StateCompleted:
		return "done"
	case core.StateFailed:
		return "failed"
	}
	if v.Transfer != nil {
		switch v.Transfer.State {
		case core.TransferStalled:
			return "stalled"
		case core.TransferInProgress:
			return "active"
		}
	}
	return "queued"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/observ/... -run TestDashboardStatus -v`
Expected: PASS (all 7 subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/observ/status.go internal/observ/status_test.go
git commit -m "feat(observ): add dashboardStatus job-state mapping"
```

---

## Task 6: GET /api/jobs endpoint

**Files:**
- Modify: `internal/observ/observ.go`
- Modify: `internal/observ/observ_test.go`

**Interfaces:**
- Consumes: `core.JobView` (Task 4), `dashboardStatus` (Task 5)
- Produces: `type JobsFunc func(ctx context.Context) ([]core.JobView, error)`; `NewServer(reg *prometheus.Registry, status StatusFunc, jobs JobsFunc, cancel CancelFunc) http.Handler` (the `CancelFunc` parameter and its route are added in Task 7 — this task adds the parameter as a no-op-accepting signature change so both tasks don't fight over the same function signature edit; Task 7 fills in its behavior). Route: `GET /api/jobs`.

- [ ] **Step 1: Write the failing test**

Add to `internal/observ/observ_test.go`:

```go
func TestJobsEndpointReturnsJobList(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet", State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferInProgress, BytesDone: 100, BytesTotal: 200},
				Peer:     "flac_hoarder",
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) (cancelResult, error) { return cancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 job, got %d", len(got))
	}
	if got[0].ID != 7 || got[0].Title != "Rounds" || got[0].Artist != "Four Tet" {
		t.Errorf("unexpected job DTO: %+v", got[0])
	}
	if got[0].Status != "active" {
		t.Errorf("Status = %q, want active", got[0].Status)
	}
	if got[0].Peer != "flac_hoarder" {
		t.Errorf("Peer = %q, want flac_hoarder", got[0].Peer)
	}
	if got[0].BytesDone != 100 || got[0].BytesTotal != 200 {
		t.Errorf("bytes = %d/%d, want 100/200", got[0].BytesDone, got[0].BytesTotal)
	}
}

func TestJobsEndpointReturns500OnStoreError(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, errors.New("db exploded") }
	cancel := func(ctx context.Context, jobID int64) (cancelResult, error) { return cancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}
```

Add `"errors"` and `"github.com/samuelenocsson/slusk/internal/core"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/observ/... -run TestJobsEndpoint -v`
Expected: FAIL — compile error, `NewServer` takes 2 args not 4, `jobDTO`/`cancelResult`/`cancelResultOK` undefined

- [ ] **Step 3: Implement the DTO, JobsFunc, CancelFunc placeholder type, and the route**

Edit `internal/observ/observ.go`. Add imports `"strconv"`, `"github.com/samuelenocsson/slusk/internal/core"`. Add types and update `NewServer`:

```go
// jobDTO is the JSON shape served at /api/jobs — a flattened, display-ready
// view of core.JobView so the frontend never needs to know about the
// engine's internal state machine.
type jobDTO struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Status     string `json:"status"`
	Peer       string `json:"peer"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	UpdatedAt  string `json:"updatedAt"`
}

func toJobDTO(v core.JobView) jobDTO {
	d := jobDTO{
		ID:        v.Job.ID,
		Title:     v.Job.Title,
		Artist:    v.Job.ArtistName,
		Status:    dashboardStatus(v),
		Peer:      v.Peer,
		UpdatedAt: v.Job.UpdatedAt.Format(timeFormat),
	}
	if v.Transfer != nil {
		d.BytesDone = v.Transfer.BytesDone
		d.BytesTotal = v.Transfer.BytesTotal
	}
	return d
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

// JobsFunc produces the current list of job views (typically backed by the
// store's ListJobsWithTransfer).
type JobsFunc func(ctx context.Context) ([]core.JobView, error)

// cancelResult is the outcome of a CancelFunc call.
type cancelResult int

const (
	cancelResultOK cancelResult = iota
	cancelResultNotFound
	cancelResultFailed
)

// CancelFunc cancels a job by id, returning which outcome occurred.
type CancelFunc func(ctx context.Context, jobID int64) (cancelResult, error)
```

Update `NewServer`:

```go
// NewServer returns an http.Handler exposing /metrics, /status, /api/jobs,
// /api/jobs/{id}/cancel, and the dashboard UI at /.
func NewServer(reg *prometheus.Registry, status StatusFunc, jobs JobsFunc, cancel CancelFunc) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		report, err := status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		views, err := jobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dtos := make([]jobDTO, len(views))
		for i, v := range views {
			dtos[i] = toJobDTO(v)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dtos)
	})
	return mux
}
```

`strconv` isn't used yet in this task (it lands in Task 7 for parsing the `{id}` path segment) — do not add an unused import; add it in Task 7 instead.

- [ ] **Step 4: Fix the two existing tests' NewServer calls**

`TestStatusEndpointReturnsReport` and `TestMetricsEndpointServes` in `internal/observ/observ_test.go` call `NewServer(reg, status)` with two args — update both call sites to pass placeholder `jobs`/`cancel` funcs:

```go
jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
cancel := func(ctx context.Context, jobID int64) (cancelResult, error) { return cancelResultOK, nil }
h := NewServer(reg, status, jobs, cancel)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/observ/... -v`
Expected: PASS (all tests, including the two new ones and the two fixed ones)

- [ ] **Step 6: Update main.go's NewServer call to compile**

`cmd/slusk/main.go:79` currently calls `observ.NewServer(reg, statusFn)`. This will fail to compile until Task 8 adds the `jobsFn`/`cancelFn` closures. For now, add temporary inline closures so the build is green after this task — Task 8 replaces them with the real implementations:

```go
jobsFn := func(ctx context.Context) ([]core.JobView, error) { return st.ListJobsWithTransfer(ctx) }
cancelFn := func(ctx context.Context, jobID int64) (observ.CancelResultPlaceholder, error) { return 0, nil } // replaced in Task 8
```

Actually — do not use a placeholder type that doesn't exist. Instead, in this step only add the real `jobsFn` (which is simple and fully specified now) and leave `cancelFn` as `nil`-safe by deferring this whole main.go edit to Task 8, where both closures are written together with the full cancel logic. **Skip editing main.go in this task.** Confirm `cmd/slusk` currently fails to build (expected, temporary) and note it will be fixed in Task 8:

Run: `go build ./... 2>&1 | grep slusk` — expect an error on `cmd/slusk/main.go` about `not enough arguments in call to observ.NewServer`. This is expected and resolved in Task 8; do not fix it here.

- [ ] **Step 7: Commit**

```bash
git add internal/observ/observ.go internal/observ/observ_test.go
git commit -m "feat(observ): add GET /api/jobs endpoint"
```

Note: `cmd/slusk` will not build until Task 8. This is intentional — `internal/observ` is fully tested and correct in isolation via `go test ./internal/observ/...`.

---

## Task 7: POST /api/jobs/{id}/cancel endpoint

**Files:**
- Modify: `internal/observ/observ.go`
- Modify: `internal/observ/observ_test.go`

**Interfaces:**
- Consumes: `CancelFunc` (Task 6's type), `cancelResult`/`cancelResultOK`/`cancelResultNotFound`/`cancelResultFailed` (Task 6)
- Produces: route `POST /api/jobs/{id}/cancel` — consumed by Task 8's real `cancelFn` wiring and Task 10's frontend JS.

- [ ] **Step 1: Write the failing tests**

Add to `internal/observ/observ_test.go`:

```go
func TestCancelEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	var gotID int64
	cancel := func(ctx context.Context, jobID int64) (cancelResult, error) {
		gotID = jobID
		return cancelResultOK, nil
	}
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("cancel called with id %d, want 42", gotID)
	}
}

func TestCancelEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (cancelResult, error) { return cancelResultNotFound, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestCancelEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (cancelResult, error) { return cancelResultFailed, errors.New("advance failed") }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status code = %d, want 502", rec.Code)
	}
}

func TestCancelEndpointBadID(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (cancelResult, error) { return cancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/observ/... -run TestCancelEndpoint -v`
Expected: FAIL — 404 (route not registered) on all four

- [ ] **Step 3: Implement the route**

Edit `internal/observ/observ.go`. Add `"strconv"` to imports. Add the route inside `NewServer`, after the `/api/jobs` handler:

```go
	mux.HandleFunc("/api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid job id", http.StatusBadRequest)
			return
		}
		result, err := cancel(r.Context(), jobID)
		switch result {
		case cancelResultNotFound:
			http.Error(w, "job not found", http.StatusNotFound)
		case cancelResultFailed:
			msg := "cancel failed"
			if err != nil {
				msg = err.Error()
			}
			http.Error(w, msg, http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
```

(`http.ServeMux`'s `{id}` wildcard pattern and `r.PathValue` require Go 1.22+ — confirm with `go version` and check `go.mod`'s `go` directive is `>= 1.22` before relying on this; if it's older, use `r.URL.Path` + `strings.TrimSuffix`/`strings.TrimPrefix` parsing instead and note the deviation in the commit message.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/observ/... -v`
Expected: PASS (all tests)

- [ ] **Step 5: Commit**

```bash
git add internal/observ/observ.go internal/observ/observ_test.go
git commit -m "feat(observ): add POST /api/jobs/{id}/cancel endpoint"
```

---

## Task 8: Wire real jobsFn/cancelFn in main.go

**Files:**
- Modify: `cmd/slusk/main.go`

**Interfaces:**
- Consumes: `store.Store.ListJobsWithTransfer`, `store.Store.JobWithTransfer`, `store.Store.AdvanceJobState` (all pre-existing or from Task 4), `slskd.Client.Cancel` (pre-existing, `internal/slskd/client.go:188`), `observ.JobsFunc`, `observ.CancelFunc`, `cancelResultOK`/`cancelResultNotFound`/`cancelResultFailed` (Task 6/7 — note these are unexported in `observ`, so `main.go` cannot construct them directly; see Step 1).
- Produces: a building, fully-wired `cmd/slusk` binary.

- [ ] **Step 1: Export cancel result constants from observ**

`cancelResult` and its constants are currently unexported (Task 6), which blocks `main.go` (a different package) from returning them. Edit `internal/observ/observ.go` to export them — rename `cancelResult` → `CancelResult`, `cancelResultOK` → `CancelResultOK`, `cancelResultNotFound` → `CancelResultNotFound`, `cancelResultFailed` → `CancelResultFailed`. Update every reference in `observ.go` and `observ_test.go` (both the type/route code from Task 6/7 and all four test functions from Task 7 plus the two from Task 6) to use the exported names.

Run: `go test ./internal/observ/... -v`
Expected: PASS (renaming only, behavior unchanged)

Commit this rename on its own:

```bash
git add internal/observ/observ.go internal/observ/observ_test.go
git commit -m "refactor(observ): export CancelResult type and constants"
```

- [ ] **Step 2: Write the cancelFn closure in main.go**

Edit `cmd/slusk/main.go`. Add `"github.com/samuelenocsson/slusk/internal/core"` to imports. Replace the `statusFn`-and-server block (currently lines 72-79) with:

```go
	statusFn := func(ctx context.Context) (observ.StatusReport, error) {
		active, err := st.ActiveTransfers(ctx)
		if err != nil {
			return observ.StatusReport{}, err
		}
		return observ.StatusReport{Active: len(active)}, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return st.ListJobsWithTransfer(ctx)
	}
	cancelFn := func(ctx context.Context, jobID int64) (observ.CancelResult, error) {
		view, found, err := st.JobWithTransfer(ctx, jobID)
		if err != nil {
			return observ.CancelResultFailed, err
		}
		if !found {
			return observ.CancelResultNotFound, nil
		}
		if view.Transfer != nil && view.Transfer.SlskdID != "" {
			if err := peers.Cancel(ctx, view.Transfer.Username, view.Transfer.SlskdID); err != nil {
				logger.Warn("slskd cancel failed, still advancing job state", "job_id", jobID, "err", err)
			}
		}
		if err := st.AdvanceJobState(ctx, jobID, core.StateCancelled, time.Now()); err != nil {
			return observ.CancelResultFailed, err
		}
		return observ.CancelResultOK, nil
	}
	srv := &http.Server{Addr: cfg.Observ.ListenAddr, Handler: observ.NewServer(reg, statusFn, jobsFn, cancelFn)}
```

Add `"time"` to imports if not already present (check the existing import block — `main.go` does not currently import `time` directly; `time.Now()` is needed here).

- [ ] **Step 3: Build and verify**

Run: `go build ./...`
Expected: no errors

- [ ] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: PASS across all packages

- [ ] **Step 5: Commit**

```bash
git add cmd/slusk/main.go
git commit -m "feat(cmd): wire dashboard jobsFn/cancelFn into observ server"
```

---

## Task 9: Dashboard HTML template and embedding

**Files:**
- Create: `internal/observ/web/dashboard.html`
- Create: `internal/observ/web.go`
- Create: `internal/observ/web_test.go`
- Modify: `internal/observ/observ.go`

**Interfaces:**
- Consumes: nothing new
- Produces: `GET /` serves the dashboard shell; `func dashboardTemplate() *template.Template` used only within `internal/observ`; Task 10 relies on the DOM element IDs defined here (`#stat-cards`, `#queue-body`, `#view-overview`, `#view-queue`, nav buttons with `data-view` attributes) matching exactly.

- [ ] **Step 1: Write the failing test**

Create `internal/observ/web_test.go`:

```go
package observ

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slusk/internal/core"
)

func TestRootServesDashboardHTML(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx interface{ Done() <-chan struct{} }) {} // placeholder, replaced below
	_ = status
	h := newTestHandler(reg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="view-overview"`) {
		t.Error("body missing #view-overview")
	}
	if !strings.Contains(body, `id="view-queue"`) {
		t.Error("body missing #view-queue")
	}
	if !strings.Contains(body, "/dashboard.js") {
		t.Error("body does not reference /dashboard.js")
	}
}

func TestDashboardJSServed(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newTestHandler(reg)

	req := httptest.NewRequest(http.MethodGet, "/dashboard.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want javascript", ct)
	}
}
```

This test file uses a `newTestHandler` helper that doesn't exist yet and an unused placeholder — clean this up properly in Step 1 for real by adding the helper to `internal/observ/observ_test.go` (not a new file) so all tests can share it:

```go
// newTestHandler builds a NewServer with no-op status/jobs/cancel funcs, for
// tests that only care about routes unrelated to those three.
func newTestHandler(reg *prometheus.Registry) http.Handler {
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	return NewServer(reg, status, jobs, cancel)
}
```

Then rewrite `internal/observ/web_test.go` without the placeholder/unused code:

```go
package observ

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRootServesDashboardHTML(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="view-overview"`, `id="view-queue"`, "/dashboard.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestDashboardJSServed(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/dashboard.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want javascript", ct)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/observ/... -run 'TestRootServesDashboardHTML|TestDashboardJSServed' -v`
Expected: FAIL — `newTestHandler` undefined (add it to `observ_test.go` per above first), then 404s on `/` and `/dashboard.js`

- [ ] **Step 3: Create the HTML template**

Create `internal/observ/web/dashboard.html`. This is the real page shell — dark theme, IBM Plex fonts, sidebar nav, Overview stat cards + active-downloads list, Queue search/filter/table with expandable rows and a Cancel button, styled per the mockup but with all fake-data-only elements (sparklines, settings form, reconcile countdown, Hälsa nav item, Inställningar nav item) removed:

```html
<!DOCTYPE html>
<html lang="sv">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>slusk</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600;700&family=IBM+Plex+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
  :root{
    --accent:#35c48f; --active:#4f9dfb; --stalled:#e5b93d; --done:#35c48f;
    --queued:#5f6672; --orphaned:#e5595d; --failed:#e5595d;
  }
  *{box-sizing:border-box;}
  body{margin:0;background:#0b0d10;font-family:'IBM Plex Sans',sans-serif;color:#dfe2e8;}
  .app{display:flex;min-height:100vh;}
  aside{width:220px;flex:0 0 220px;background:#101317;border-right:1px solid #1e222a;display:flex;flex-direction:column;height:100vh;position:sticky;top:0;}
  .brand{display:flex;align-items:center;gap:10px;padding:18px;}
  .brand-mark{width:32px;height:32px;border-radius:8px;background:var(--accent);display:flex;align-items:center;justify-content:center;font-family:'IBM Plex Mono',monospace;font-weight:600;color:#08130d;}
  nav{padding:6px 12px;display:flex;flex-direction:column;gap:2px;}
  nav button{display:flex;align-items:center;gap:9px;padding:9px 10px;border:none;background:transparent;color:#8a919d;font-size:13px;border-radius:8px;cursor:pointer;text-align:left;}
  nav button.active{background:rgba(53,196,143,0.12);color:var(--accent);}
  main{flex:1;min-width:0;padding:22px 26px 48px;}
  h1{margin:0 0 4px;font-size:18px;font-weight:600;}
  .view{display:none;}
  .view.active{display:block;}
  .stat-cards{display:grid;grid-template-columns:repeat(4,1fr);gap:13px;margin-bottom:16px;}
  .card{background:#14171c;border:1px solid #21252e;border-radius:11px;padding:14px 15px;}
  .card .label{font-size:12px;color:#8a919d;}
  .card .value{font-family:'IBM Plex Mono',monospace;font-size:28px;font-weight:600;margin-top:6px;}
  table{width:100%;border-collapse:collapse;font-size:12.5px;background:#14171c;border:1px solid #21252e;border-radius:11px;overflow:hidden;}
  th{text-align:left;padding:9px 14px;font-size:11px;text-transform:uppercase;color:#6b7280;border-bottom:1px solid #21252e;}
  td{padding:10px 14px;border-bottom:1px solid #191d24;}
  tr.job-row{cursor:pointer;}
  tr.job-row:hover{background:#181c22;}
  .pill{display:inline-block;padding:3px 9px;border-radius:20px;font-size:11px;font-weight:600;}
  .pill.queued{background:rgba(95,102,114,0.18);color:var(--queued);}
  .pill.active{background:rgba(79,157,251,0.15);color:var(--active);}
  .pill.stalled{background:rgba(229,185,61,0.15);color:var(--stalled);}
  .pill.done{background:rgba(53,196,143,0.15);color:var(--done);}
  .pill.failed{background:rgba(229,89,93,0.15);color:var(--failed);}
  .bar{height:5px;border-radius:3px;background:#22262e;overflow:hidden;}
  .bar-fill{height:100%;background:var(--accent);}
  .detail-row{background:#0e1115;}
  .detail-row td{padding:16px 20px;}
  button.action{padding:6px 12px;border-radius:7px;border:1px solid rgba(229,89,93,0.28);background:rgba(229,89,93,0.09);color:var(--orphaned);font-size:12px;cursor:pointer;}
  #search{padding:8px 10px;border-radius:8px;background:#13161b;border:1px solid #23272f;color:#dfe2e8;font-size:12.5px;margin-bottom:12px;width:260px;}
</style>
</head>
<body>
<div class="app">
  <aside>
    <div class="brand">
      <div class="brand-mark">sl</div>
      <div>
        <div style="font-weight:600;">slusk</div>
        <div style="font-size:11px;color:#5f6672;">Lidarr → slskd</div>
      </div>
    </div>
    <nav>
      <button data-view="overview" class="active">Översikt</button>
      <button data-view="queue">Kö</button>
    </nav>
  </aside>
  <main>
    <div id="view-overview" class="view active">
      <h1>Översikt</h1>
      <div id="stat-cards" class="stat-cards"></div>
      <table>
        <thead><tr><th>Album / Artist</th><th>Peer</th><th>Progress</th></tr></thead>
        <tbody id="overview-active-body"></tbody>
      </table>
    </div>
    <div id="view-queue" class="view">
      <h1>Kö</h1>
      <input id="search" type="text" placeholder="Sök artist, album, peer…">
      <table>
        <thead>
          <tr><th>Status</th><th>Album / Artist</th><th>Peer</th><th>Progress</th><th></th></tr>
        </thead>
        <tbody id="queue-body"></tbody>
      </table>
    </div>
  </main>
</div>
<script src="/dashboard.js"></script>
</body>
</html>
```

- [ ] **Step 4: Create web.go with the embed and handlers**

Create `internal/observ/web.go`:

```go
// Package observ: web.go embeds and serves the dashboard's static assets
// (HTML shell + JS). The template has no server-side data holes — all data
// is fetched client-side from /api/jobs so this package never needs to
// import html/template's data-binding surface beyond a single static parse.
package observ

import (
	"embed"
	"net/http"
)

//go:embed web/dashboard.html
var dashboardHTML embed.FS

//go:embed web/dashboard.js
var dashboardJS embed.FS

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	b, err := dashboardHTML.ReadFile("web/dashboard.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func dashboardJSHandler(w http.ResponseWriter, r *http.Request) {
	b, err := dashboardJS.ReadFile("web/dashboard.js")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(b)
}
```

(No `html/template` needed — the page has no server-rendered data, so a plain embedded static file is simpler and matches YAGNI. This supersedes the "Go html/template" framing from the design doc's high-level description; the design's intent — no build step, server-embedded assets — is preserved.)

- [ ] **Step 5: Create a placeholder dashboard.js so the embed compiles**

Create `internal/observ/web/dashboard.js` with a minimal stub (Task 10 replaces this with the real polling logic):

```js
console.log("slusk dashboard loading");
```

- [ ] **Step 6: Register the routes**

Edit `internal/observ/observ.go`, add to `NewServer` (after the `/api/jobs/{id}/cancel` handler):

```go
	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/dashboard.js", dashboardJSHandler)
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/observ/... -v`
Expected: PASS (all tests)

- [ ] **Step 8: Full build and test**

Run: `go build ./... && go test ./...`
Expected: no errors, all tests pass

- [ ] **Step 9: Commit**

```bash
git add internal/observ/web.go internal/observ/web_test.go internal/observ/web/dashboard.html internal/observ/web/dashboard.js internal/observ/observ.go internal/observ/observ_test.go
git commit -m "feat(observ): serve dashboard HTML shell at GET /"
```

---

## Task 10: Dashboard JS — poll and render Overview + Queue

**Files:**
- Modify: `internal/observ/web/dashboard.js`

**Interfaces:**
- Consumes: `GET /api/jobs` (Task 6, returns `jobDTO[]` as `{id, title, artist, status, peer, bytesDone, bytesTotal, updatedAt}`), `POST /api/jobs/{id}/cancel` (Task 7), the DOM structure from Task 9 (`#stat-cards`, `#overview-active-body`, `#queue-body`, `#search`, nav `button[data-view]`, `.view`/`.view.active`).
- Produces: a working browser UI. Nothing downstream depends on this file's internals — it's the leaf of the plan.

This is a JS file, not Go, so there's no `go test` cycle — verification is manual (Step 3) per the design doc's testing section ("No automated JS tests... manual verification").

- [ ] **Step 1: Write the real dashboard.js**

Replace `internal/observ/web/dashboard.js`:

```js
const STATUS_LABEL = { queued: 'Köad', active: 'Aktiv', stalled: 'Stannad', done: 'Klar', failed: 'Misslyckad' };

let jobs = [];
let searchTerm = '';

function fmtBytes(n) {
  if (!n) return '0 MB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

function pct(job) {
  if (!job.bytesTotal) return 0;
  return Math.round((job.bytesDone / job.bytesTotal) * 100);
}

async function fetchJobs() {
  try {
    const res = await fetch('/api/jobs');
    if (!res.ok) return; // keep showing last-good data on a transient error
    jobs = await res.json();
    render();
  } catch (e) {
    // network error: keep showing last-good data
  }
}

function statCards() {
  const counts = { queued: 0, active: 0, stalled: 0, done: 0, failed: 0 };
  for (const j of jobs) counts[j.status] = (counts[j.status] || 0) + 1;
  const el = document.getElementById('stat-cards');
  el.innerHTML = ['queued', 'active', 'stalled', 'done'].map(s =>
    `<div class="card"><div class="label">${STATUS_LABEL[s]}</div><div class="value">${counts[s] || 0}</div></div>`
  ).join('');
}

function overviewActiveRows() {
  const active = jobs.filter(j => j.status === 'active');
  const body = document.getElementById('overview-active-body');
  body.innerHTML = active.map(j => `
    <tr>
      <td>${escapeHtml(j.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(j.artist)}</span></td>
      <td>${escapeHtml(j.peer)}</td>
      <td><div class="bar"><div class="bar-fill" style="width:${pct(j)}%"></div></div></td>
    </tr>
  `).join('');
}

function matchesSearch(j) {
  if (!searchTerm) return true;
  const hay = (j.title + ' ' + j.artist + ' ' + j.peer).toLowerCase();
  return hay.includes(searchTerm.toLowerCase());
}

let expandedId = null;

function queueRows() {
  const filtered = jobs.filter(matchesSearch);
  const body = document.getElementById('queue-body');
  body.innerHTML = filtered.map(j => {
    const rows = [`
      <tr class="job-row" data-id="${j.id}">
        <td><span class="pill ${j.status}">${STATUS_LABEL[j.status] || j.status}</span></td>
        <td>${escapeHtml(j.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(j.artist)}</span></td>
        <td>${escapeHtml(j.peer)}</td>
        <td><div class="bar"><div class="bar-fill" style="width:${pct(j)}%"></div></div></td>
        <td></td>
      </tr>
    `];
    if (expandedId === j.id) {
      rows.push(`
        <tr class="detail-row">
          <td colspan="5">
            <div>Peer: ${escapeHtml(j.peer) || '—'}</div>
            <div>Nedladdat: ${fmtBytes(j.bytesDone)} / ${fmtBytes(j.bytesTotal)}</div>
            <button class="action" data-cancel="${j.id}">Avbryt</button>
          </td>
        </tr>
      `);
    }
    return rows.join('');
  }).join('');

  body.querySelectorAll('tr.job-row').forEach(tr => {
    tr.addEventListener('click', () => {
      const id = Number(tr.getAttribute('data-id'));
      expandedId = expandedId === id ? null : id;
      queueRows();
    });
  });
  body.querySelectorAll('button[data-cancel]').forEach(btn => {
    btn.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      const id = btn.getAttribute('data-cancel');
      await fetch(`/api/jobs/${id}/cancel`, { method: 'POST' });
      await fetchJobs();
    });
  });
}

function escapeHtml(s) {
  const div = document.createElement('div');
  div.textContent = s || '';
  return div.innerHTML;
}

function render() {
  statCards();
  overviewActiveRows();
  queueRows();
}

function setupNav() {
  document.querySelectorAll('nav button[data-view]').forEach(btn => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('nav button[data-view]').forEach(b => b.classList.remove('active'));
      document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
      btn.classList.add('active');
      document.getElementById('view-' + btn.getAttribute('data-view')).classList.add('active');
    });
  });
}

function setupSearch() {
  const input = document.getElementById('search');
  input.addEventListener('input', () => {
    searchTerm = input.value;
    queueRows();
  });
}

setupNav();
setupSearch();
fetchJobs();
setInterval(fetchJobs, 3000);
```

- [ ] **Step 2: Run the Go test suite once more to confirm the embed still compiles with real content**

Run: `go build ./... && go test ./...`
Expected: no errors, all tests pass (the embed doesn't care about JS content, just that the file exists — this step is a sanity check, not new coverage)

- [ ] **Step 3: Manual verification**

This step has no automated test — follow it exactly and report the actual observed results, not an assumption of success:

1. Ensure a config file exists (copy `config.example.toml` if needed) pointing at a real or dev slskd/Lidarr, or a store path you can seed manually.
2. Run: `go run ./cmd/slusk -config <path-to-config>`
3. In a browser, open `http://<observ.listen_addr>/`
4. Confirm the page loads with the dark theme, sidebar, and "Översikt"/"Kö" nav buttons.
5. Confirm stat cards render (even if all zero on an empty store).
6. If there are active album jobs in the store, click "Kö", confirm rows appear with title/artist/peer/status, click a row to expand it, confirm the Peer/bytes detail and an "Avbryt" button appear.
7. Click "Avbryt" on a job, confirm (after the next 3s poll) the row's status updates.
8. Open browser devtools Network tab, confirm `/api/jobs` is polled roughly every 3 seconds.

- [ ] **Step 4: Commit**

```bash
git add internal/observ/web/dashboard.js
git commit -m "feat(observ): dashboard.js polls /api/jobs and renders Overview + Queue"
```

---

## Task 11: Update docs

**Files:**
- Modify: `docs/smoke-test.md` (if it documents how to exercise the running daemon — check its current content first)

**Interfaces:**
- Consumes: nothing
- Produces: nothing consumed elsewhere; documentation only.

- [ ] **Step 1: Read the current smoke-test doc**

Run: `cat docs/smoke-test.md`

- [ ] **Step 2: Add a dashboard verification step**

If the doc walks through starting the daemon and checking `/status`/`/metrics`, add a step in the same style pointing at `http://<observ.listen_addr>/` for the dashboard and describing what to expect (stat cards, queue table, cancel action) — matching Task 10 Step 3's manual verification list. Write the actual added section based on the file's real existing structure and tone; do not guess its format sight-unseen — read it first per Step 1.

- [ ] **Step 3: Commit**

```bash
git add docs/smoke-test.md
git commit -m "docs: add dashboard verification to smoke test"
```

---

## Plan Self-Review Notes

- **Spec coverage:** Overview view (stat cards + active downloads list) → Tasks 6, 9, 10. Queue view (table, search, expand, Cancel) → Tasks 6, 7, 9, 10. Schema/title-artist caching → Tasks 1-3. `ListJobsWithTransfer` → Task 4. Served from existing `observ.Server` → Tasks 6-9 all extend `internal/observ`, no new server/port. No new deps, no build step → Task 9 deliberately uses a static embedded file instead of `html/template` data-binding, since the page needs no server-side data holes (all data is client-fetched JSON) — this is a simplification over the design doc's literal "Go html/template" phrasing but satisfies its actual intent (server-embedded, no build step) and was flagged inline in Task 9 Step 4 rather than silently deviating.
- **Deferred-per-spec items confirmed absent from this plan:** Hälsa view, Inställningar view, Filer/Historik detail, Retry/Tvinga sökning actions, per-row orphaned detail — none appear in any task above, matching the design doc's explicit out-of-scope list.
- **Known intentional temporary breakage:** `cmd/slusk` does not build between Task 6 and Task 8 (Task 6 changes `observ.NewServer`'s signature; Task 8 updates the only caller). This is flagged explicitly in Task 6 Step 6 so the executing agent doesn't mistake it for a real bug. Task 8 Step 3 confirms the build is green again.
- **Type/signature consistency check:** `core.JobView` (Task 4) is used identically in Tasks 5, 6, 7, 8. `dashboardStatus(v core.JobView) string` (Task 5) is called from `toJobDTO` (Task 6) with no signature drift. `CancelResult`/`CancelResultOK`/`CancelResultNotFound`/`CancelResultFailed` are introduced unexported in Task 6, used unexported through Task 7, then exported in Task 8 Step 1 with every reference updated in the same step — no task after Task 8 uses the lowercase names.
