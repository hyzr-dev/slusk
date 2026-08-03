# Pipeline State Machine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single-goroutine `internal/engine` with `internal/pipeline`: five independent per-state worker modules (WantedSync, Discovery, Selecting, Downloading, Importing), each its own goroutine with its own tick, coordinating only through Postgres, with a persistent candidate cache and exponential retry backoff.

**Architecture:** State = which queue a job sits in; exactly one module writes transitions out of each non-terminal state (exception: WantedSync cancels from any state). Backoff is data (`not_before`), not a state. A candidate is its own attempt: `NEW → ACTIVE → SUCCEEDED|FAILED`, with the search-result file list cached as JSONB so Selecting can enqueue long after Discovery searched. Spec: `docs/superpowers/specs/2026-07-06-pipeline-state-machine-design.md` — read it before starting.

**Tech Stack:** Go, PostgreSQL (pgx/stdlib), embedded-postgres for tests (`internal/store/storetest`), existing clients `internal/lidarr`, `internal/slskd`, ranking in `internal/matcher`.

## Global Constraints

- Conversation with Samuel is in Swedish; **all code, comments, commit messages in English**.
- One PR, one commit per task. Every commit must build (`go build ./...`) with green tests (`go test ./...`).
- The old `internal/engine` package must keep compiling and passing its tests until Task 12 deletes it. Until then all schema/store changes are strictly additive (plus one FK drop, see Task 2).
- States (exact strings): `WANTED`, `SELECTING`, `DOWNLOADING`, `IMPORTING`, `DONE`, `CANCELLED`, `FAILED`. Candidate states: `NEW`, `ACTIVE`, `SUCCEEDED`, `FAILED`.
- Config defaults (exact values from spec): `max_retries = 10`, `backoff_base = "15m"`, `backoff_cap = "24h"`, `candidate_ttl = "24h"`, `failed_revive_after = "720h"`, `stuck_after = "1h"`, `tick_timeout = "5m"`, `wanted_sync_interval = "15m"`, `discovery_interval = "30s"`, `selecting_interval = "10s"`, `downloading_interval = "15s"`, `importing_interval = "30s"`.
- Per-candidate failures (download/import) do NOT touch `retries` — immediate return to SELECTING. Only search-cycle failures (empty search, exhausted candidates) increment `retries`; hitting `max_retries` → FAILED with `failed_at` set. Stale cache (TTL) → WANTED **without** incrementing `retries`.
- Peer reliability invariant (verbatim from today): `known_users`/`artist_user_reliability` are written incrementally via `RecordAttemptOutcome` at candidate completion and NEVER derived from the candidates table.
- Two-phase fail cleanup in Downloading is ported **verbatim in behaviour** from `engine/discovery.go:568-669` — do not redesign it.
- Every module's DB reads for runnable jobs filter `(not_before IS NULL OR not_before <= now)`.
- `<summary>`-style doc comments: this is Go — follow the repo's existing dense doc-comment style (every exported symbol documented with the *why*).

## File Structure

```
internal/core/state.go          (modify) new job/candidate state constants
internal/core/models.go         (modify) AlbumJob new fields; Candidate, CandidateFile types
internal/store/schema.sql       (modify) candidates table, album_jobs columns, FK drop
internal/store/candidates.go    (create)  candidate CRUD + transactional activation
internal/store/pipeline.go      (modify) runnable-job queries, backoff/fail/revive/cancel helpers
internal/pipeline/ports.go      (create)  consumer-declared interfaces (pattern: engine/ports.go)
internal/pipeline/runner.go     (create)  Module interface, per-module goroutine loop, health
internal/pipeline/backoff.go    (create)  nextBackoff + failOrBackoff helper
internal/pipeline/wanted.go     (create)  WantedSync module
internal/pipeline/discovery.go  (create)  Discovery module (search + score + cache)
internal/pipeline/selecting.go  (create)  Selecting module (pick candidate, MaxActive gate)
internal/pipeline/downloading.go(create)  Downloading module (reconcile absorbed + resolve + top-up)
internal/pipeline/importing.go  (create)  Importing module (verify gate + import + confirm)
internal/pipeline/query.go      (create)  moved verbatim from engine/query.go (normalizeQuery)
internal/pipeline/paths.go      (create)  moved verbatim from engine/paths.go (AlbumFolder, commonLeaf)
internal/config/config.go       (modify) [pipeline] section
cmd/slusk/main.go            (modify) wire pipeline instead of engine
internal/observ/*               (modify) state names, retries/not_before, retry endpoint, per-module health
internal/engine/                (delete in Task 12)
```

Each module file contains its params struct, constructor, `Tick`, and its private helpers — nothing shared between modules except `runner.go`, `backoff.go`, `ports.go`, `query.go`, `paths.go`.

---

### Task 1: Core types — new states and candidate model

**Files:**
- Modify: `internal/core/state.go`
- Modify: `internal/core/models.go`
- Test: `internal/core/state_test.go`

**Interfaces:**
- Produces: `core.StateWanted`, `core.StateDone`, `AlbumJobState.PipelineTerminal()`, `core.CandidateState` + constants, `core.Candidate`, `core.CandidateFile`, updated `core.AlbumJob` fields `Retries int`, `NotBefore *time.Time`, `FailedAt *time.Time`.

The old engine still uses the old constants; add new ones alongside. Name collision: `StateSelecting` and `StateDownloading` etc. already exist with the string values we want (`"SELECTING"`, `"DOWNLOADING"`, `"IMPORTING"`, `"FAILED"`, `"CANCELLED"`) — **reuse them**. Only two new job states are needed:

- [ ] **Step 1: Write the failing test** — append to `internal/core/state_test.go`:

```go
func TestPipelineTerminalStates(t *testing.T) {
	for _, s := range []AlbumJobState{StateDone, StateCancelled, StateFailed} {
		if !s.PipelineTerminal() {
			t.Errorf("%s should be pipeline-terminal", s)
		}
	}
	for _, s := range []AlbumJobState{StateWanted, StateSelecting, StateDownloading, StateImporting} {
		if s.PipelineTerminal() {
			t.Errorf("%s should not be pipeline-terminal", s)
		}
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/core/ -run TestPipelineTerminalStates -v` — expected: FAIL (undefined: StateDone, StateWanted, PipelineTerminal)

- [ ] **Step 3: Implement** in `internal/core/state.go`:

```go
const (
	// StateWanted is the pipeline rewrite's entry state: synced from Lidarr's
	// wanted list, awaiting a Discovery search. Replaces DISCOVERED/SEARCHING.
	StateWanted AlbumJobState = "WANTED"
	// StateDone replaces COMPLETED in the pipeline rewrite.
	StateDone AlbumJobState = "DONE"
)

// PipelineTerminal reports whether the state is an end state in the pipeline
// state machine (spec 2026-07-06). FAILED is included: it is only ever left
// via WantedSync's revival or a manual dashboard retry, never by a module's
// normal advance. Distinct from Terminal() (the legacy engine's notion) until
// the engine is deleted.
func (s AlbumJobState) PipelineTerminal() bool {
	return s == StateDone || s == StateCancelled || s == StateFailed
}
```

And in `internal/core/models.go` add to `AlbumJob` (after `ArtistID`):

```go
	// Retries counts search-cycle failures (empty search, candidates
	// exhausted). Per-candidate failures do not touch it. At max_retries the
	// job goes FAILED. Reset to 0 when a search yields candidates.
	Retries int
	// NotBefore hides the job from every pipeline module until it passes.
	// Backoff lives here as data — there is no COOLDOWN state.
	NotBefore *time.Time
	// FailedAt is set when the job enters FAILED; WantedSync revives the job
	// once it is older than failed_revive_after and the album is still wanted.
	FailedAt *time.Time
```

Add the candidate model:

```go
// CandidateFile is one file of a cached search result, persisted as JSONB on
// the candidate so Selecting can enqueue it long after the search happened.
type CandidateFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// Candidate is one ranked Soulseek user cached for an album. A candidate is
// its own attempt: NEW (cached, untried) → ACTIVE (picked by Selecting) →
// SUCCEEDED | FAILED. Replaces CandidateAttempt in the pipeline rewrite.
type Candidate struct {
	ID                int64
	AlbumJobID        int64
	Username          string
	Score             float64
	Files             []CandidateFile
	State             CandidateState
	FailReason        string
	ImportSubmittedAt *time.Time // set by Importing after ExecuteManualImport; gates verify vs confirm phase
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
```

And in `state.go`:

```go
// CandidateState is the lifecycle of one cached candidate (see core.Candidate).
type CandidateState string

const (
	CandidateNew       CandidateState = "NEW"
	CandidateActive    CandidateState = "ACTIVE"
	CandidateSucceeded CandidateState = "SUCCEEDED"
	CandidateFailed    CandidateState = "FAILED"
)
```

- [ ] **Step 4: Run** `go test ./internal/core/ -v` — expected: PASS. Also `go build ./...` — engine must still compile.

- [ ] **Step 5: Commit** `git commit -m "feat(core): pipeline job states, retries/not_before fields, candidate model"`

---

### Task 2: Schema + store — candidates table and pipeline queries

**Files:**
- Modify: `internal/store/schema.sql`
- Create: `internal/store/candidates.go`
- Modify: `internal/store/pipeline.go`
- Test: `internal/store/candidates_test.go`, extend `internal/store/pipeline_test.go`

**Interfaces:**
- Consumes: Task 1's `core.Candidate`, `core.CandidateFile`, `core.CandidateState`, new `AlbumJob` fields.
- Produces (exact signatures — the module tasks' port interfaces bind to these):

```go
// candidates.go
type NewCandidate struct {
	Username string
	Score    float64
	Files    []core.CandidateFile
}
func (s *Store) InsertCandidates(ctx context.Context, jobID int64, cands []NewCandidate, now time.Time) error
func (s *Store) NextNewCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error) // highest score first
func (s *Store) ActiveCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error)
func (s *Store) FailCandidate(ctx context.Context, candidateID int64, reason string, now time.Time) error
func (s *Store) SucceedCandidate(ctx context.Context, candidateID int64, now time.Time) error
func (s *Store) MarkImportSubmitted(ctx context.Context, candidateID int64, now time.Time) error
// ActivateCandidate atomically (single tx): re-checks the job is still in
// SELECTING, counts jobs in DOWNLOADING+IMPORTING, and if < maxActive sets the
// candidate ACTIVE and the job DOWNLOADING. Returns false when the cap is full
// or the job left SELECTING — the caller just moves on.
func (s *Store) ActivateCandidate(ctx context.Context, candidateID, jobID int64, maxActive int, now time.Time) (bool, error)
func (s *Store) TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error)

// pipeline.go additions
// RunnableJobsInState is JobsInState plus the not_before filter. Order:
// release_date DESC for WANTED (spec: newest releases first), updated_at ASC
// otherwise (fairness FIFO).
func (s *Store) RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
// SetJobBackoff bumps retries and hides the job until notBefore. State unchanged.
func (s *Store) SetJobBackoff(ctx context.Context, jobID int64, retries int, notBefore time.Time, now time.Time) error
// MarkJobFailed: state→FAILED, failed_at=now, not_before cleared.
func (s *Store) MarkJobFailed(ctx context.Context, jobID int64, now time.Time) error
// ResetJobToWanted deletes the job's candidates and returns it to WANTED in
// one tx. retries/notBefore are written as given (exhaustion passes bumped
// values; TTL expiry and manual retry pass job.Retries-as-is / 0 and nil).
func (s *Store) ResetJobToWanted(ctx context.Context, jobID int64, retries int, notBefore *time.Time, now time.Time) error
// AdvanceJobStateFrom is the conditional transition every module uses:
// UPDATE ... SET state=$to WHERE id=$id AND state=$from. Returns whether a row
// changed — false means someone (WantedSync cancel) got there first; move on.
func (s *Store) AdvanceJobStateFrom(ctx context.Context, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error)
// CancelJobsNotWanted cancels every non-pipeline-terminal job whose album is
// absent from wantedIDs. Returns count.
func (s *Store) CancelJobsNotWanted(ctx context.Context, wantedIDs []int64, now time.Time) (int, error)
// ReviveFailedJobs returns FAILED jobs with failed_at < cutoff AND album still
// in wantedIDs to WANTED with retries=0, not_before=NULL, failed_at=NULL.
func (s *Store) ReviveFailedJobs(ctx context.Context, wantedIDs []int64, cutoff time.Time, now time.Time) (int, error)
// UpsertWantedJob is UpsertDiscoveredJob but inserting state WANTED.
func (s *Store) UpsertWantedJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error)
```

- [ ] **Step 1: Schema changes** in `internal/store/schema.sql` (append; everything idempotent like the rest of the file):

```sql
-- Pipeline rewrite (spec 2026-07-06): backoff/retry live on the job as data,
-- not as states. failed_at drives WantedSync's 30-day revival of FAILED jobs.
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS retries    BIGINT NOT NULL DEFAULT 0;
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS not_before TIMESTAMPTZ;
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS failed_at  TIMESTAMPTZ;

-- candidates replaces candidate_attempts: a candidate is its own attempt
-- (NEW → ACTIVE → SUCCEEDED|FAILED), with the search result's file list cached
-- as JSONB so Selecting can enqueue without re-searching. candidate_attempts
-- stays until the legacy engine is deleted (clean-slate deploy wipes both).
CREATE TABLE IF NOT EXISTS candidates (
    id                  BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    album_job_id        BIGINT NOT NULL REFERENCES album_jobs(id),
    username            TEXT NOT NULL,
    score               DOUBLE PRECISION NOT NULL,
    files               JSONB NOT NULL,
    state               TEXT NOT NULL,
    fail_reason         TEXT NOT NULL DEFAULT '',
    import_submitted_at TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_candidates_job ON candidates(album_job_id, state);

-- transfers.attempt_id now holds candidate IDs in the pipeline; the FK to
-- candidate_attempts must go or pipeline writes would violate it. The column
-- itself is renamed to candidate_id in the engine-deletion step.
ALTER TABLE transfers DROP CONSTRAINT IF EXISTS transfers_attempt_id_fkey;
```

- [ ] **Step 2: Write failing tests** in `internal/store/candidates_test.go` (same shape as existing store tests: `storetest.Run` TestMain already exists in the package; open a fresh store per test via the package's existing helper — read `store_test.go` first and copy its setup pattern). Cover at minimum:

```go
func TestCandidateLifecycle(t *testing.T) {
	// InsertCandidates(3 candidates, scores 1.0/3.0/2.0)
	// → NextNewCandidate returns the 3.0 one
	// → ActivateCandidate(maxActive=5) returns true; job now DOWNLOADING; candidate ACTIVE
	// → ActiveCandidate returns it; Files round-trips through JSONB intact
	// → FailCandidate → NextNewCandidate returns the 2.0 one
}

func TestActivateCandidateRespectsMaxActive(t *testing.T) {
	// two jobs in DOWNLOADING already, maxActive=2 → ActivateCandidate returns false,
	// job still SELECTING, candidate still NEW
}

func TestActivateCandidateBouncesWhenJobLeftSelecting(t *testing.T) {
	// job moved to CANCELLED between read and activate → returns false
}

func TestResetJobToWantedDeletesCandidates(t *testing.T) {
	// insert candidates, ResetJobToWanted(retries=3, notBefore=+1h)
	// → state WANTED, retries 3, not_before set, zero candidates rows
}

func TestRunnableJobsFiltersNotBefore(t *testing.T) {
	// two WANTED jobs, one with not_before in the future → only the other returned;
	// order: release_date DESC
}

func TestCancelJobsNotWanted(t *testing.T) {
	// jobs in WANTED, DOWNLOADING, DONE; wantedIDs covers none
	// → WANTED+DOWNLOADING become CANCELLED, DONE untouched, count==2
}

func TestReviveFailedJobs(t *testing.T) {
	// FAILED job failed_at 31 days ago + still wanted → WANTED, retries 0, failed_at NULL
	// FAILED job failed_at 1 day ago → untouched
	// FAILED job not in wantedIDs → untouched
}
```

- [ ] **Step 3: Run** `go test ./internal/store/ -run 'TestCandidate|TestResetJobToWanted|TestRunnable|TestCancelJobsNotWanted|TestReviveFailed' -v` — expected: FAIL (methods undefined)

- [ ] **Step 4: Implement** `candidates.go` and the `pipeline.go` additions. Notes that matter:
  - `files` marshals via `encoding/json` into/out of JSONB.
  - `NextNewCandidate`: `WHERE album_job_id=$1 AND state='NEW' ORDER BY score DESC, id ASC LIMIT 1`.
  - `ActivateCandidate` tx: `SELECT COUNT(*) FROM album_jobs WHERE state IN ('DOWNLOADING','IMPORTING')`; then `UPDATE album_jobs SET state='DOWNLOADING', updated_at=$now WHERE id=$job AND state='SELECTING'` (rowsAffected==0 → rollback, return false); then `UPDATE candidates SET state='ACTIVE', updated_at=$now WHERE id=$cand AND state='NEW'`.
  - `CancelJobsNotWanted` / `ReviveFailedJobs` pass `wantedIDs` as a Postgres array (`= ANY($1)` / `<> ALL($1)`) — see how existing store code binds slices; use `pgx`-compatible `pq`-style?? No: the repo uses `database/sql` with pgx stdlib — bind arrays with `pgx`'s array support via `github.com/jackc/pgx/v5/pgtype` or simplest: `ANY(string_to_array(...))` is ugly; check `reliability.go:85` `ReliabilityFor` — it already binds a `[]string`; copy that exact mechanism for `[]int64`.
  - Scan helpers for the new `AlbumJob` fields: extend the package's row-scan helper (grep `scanJob` / the SELECT column list in `pipeline.go`) so `retries`, `not_before`, `failed_at` come back on every job read.

- [ ] **Step 5: Run** `go test ./internal/store/ ./internal/engine/ -count=1` — expected: PASS (engine untouched but re-run to prove the FK drop broke nothing)

- [ ] **Step 6: Commit** `git commit -m "feat(store): candidates table and pipeline job queries"`

---

### Task 3: Runner — Module interface, per-module goroutines, health

**Files:**
- Create: `internal/pipeline/runner.go`
- Test: `internal/pipeline/runner_test.go`

**Interfaces:**
- Produces:

```go
package pipeline

type Module interface {
	Name() string
	Interval() time.Duration
	Tick(ctx context.Context, now time.Time) error
}

type Runner struct{ /* modules, logger, tickTimeout, per-module lastTick atomics */ }
func NewRunner(logger *slog.Logger, tickTimeout time.Duration, modules ...Module) *Runner
// Run starts one goroutine per module (immediate first tick, then on the
// module's Interval), blocks until ctx cancel, waits for all to stop.
func (r *Runner) Run(ctx context.Context) error
// Health reports each module's last completed tick, keyed by Name(). Zero
// time = never ticked. Used by /healthz: a module is stale past
// Interval()*3 + tickTimeout.
func (r *Runner) Health() map[string]time.Time
// Healthy reports whether every module has ticked within its own staleness
// window. False until every module has completed at least one tick.
func (r *Runner) Healthy() bool
```

Behaviour requirements: per-tick `context.WithTimeout(ctx, tickTimeout)`; tick errors logged (`logger.Error("tick failed", "module", m.Name(), "err", err)`) and swallowed; a panic in Tick is recovered, logged, and the loop continues (one module must never kill the process or the other modules); `lastTick` updated after every completed tick **including failed ones** (health measures liveness, not success — a persistently erroring module is visible in logs/metrics, while a hung one trips /healthz).

- [ ] **Step 1: Write failing tests** (`runner_test.go`, no DB needed — fake modules):

```go
type tickRecorder struct {
	name     string
	interval time.Duration
	ticks    atomic.Int64
	err      error
	panics   bool
	block    chan struct{} // non-nil: Tick blocks until ctx done (hang simulation)
}
// implement Module...

func TestRunnerTicksEveryModuleIndependently(t *testing.T)
// two modules, 10ms interval; run 100ms; both tick counts ≥ 5

func TestRunnerSurvivesPanicAndError(t *testing.T)
// panicking module + erroring module + healthy module; all keep ticking; process alive

func TestRunnerHealthyReflectsStaleModule(t *testing.T)
// use a tiny tickTimeout (10ms) and a Tick that ignores ctx and sleeps 500ms:
// its lastTick never advances, so Healthy() is false even though a fast
// sibling module keeps ticking

func TestRunnerStopsOnContextCancel(t *testing.T)
// cancel ctx; Run returns within 1s
```

- [ ] **Step 2: Run** `go test ./internal/pipeline/ -v` — FAIL (package empty)
- [ ] **Step 3: Implement** `runner.go`. Keep it under ~150 lines: `sync.WaitGroup`, one `func (r *Runner) loop(ctx, m Module)` with `time.Ticker`, deferred recover inside an inner `runTick` func, `atomic.Int64` UnixNano per module for lastTick.
- [ ] **Step 4: Run** `go test ./internal/pipeline/ -race -count=1` — PASS (race detector mandatory here)
- [ ] **Step 5: Commit** `git commit -m "feat(pipeline): module runner with per-module goroutines and health"`

---

### Task 4: Ports + backoff helper

**Files:**
- Create: `internal/pipeline/ports.go`, `internal/pipeline/backoff.go`
- Create: `internal/pipeline/query.go`, `internal/pipeline/paths.go` (copies)
- Test: `internal/pipeline/backoff_test.go`, `internal/pipeline/query_test.go`, `internal/pipeline/paths_test.go`

**Interfaces:**
- Consumes: store methods from Task 2.
- Produces: `MusicSource`, `PeerSearcher`, `PeerNetwork` (copy the interface bodies verbatim from `engine/ports.go:17-49` — same methods, new package), plus `Store` interfaces per module declared next to each module in later tasks (Go style: consumer declares). Here declare only what backoff needs:

```go
// backoff.go
// nextBackoff returns base * 2^retries capped at cap. retries is the value
// AFTER incrementing (first failure → retries 1 → 2*base? no: see test).
func nextBackoff(retries int, base, cap time.Duration) time.Duration

// BackoffStore is the store slice failOrBackoff needs.
type BackoffStore interface {
	SetJobBackoff(ctx context.Context, jobID int64, retries int, notBefore time.Time, now time.Time) error
	MarkJobFailed(ctx context.Context, jobID int64, now time.Time) error
	ResetJobToWanted(ctx context.Context, jobID int64, retries int, notBefore *time.Time, now time.Time) error
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
}

// failOrBackoff records one search-cycle failure: retries+1 >= maxRetries →
// MarkJobFailed; otherwise back off exponentially. resetToWanted selects
// whether the job also returns to WANTED (Selecting exhaustion) or stays put
// (Discovery empty search — already WANTED).
func failOrBackoff(ctx context.Context, st BackoffStore, log *slog.Logger, job core.AlbumJob, maxRetries int, base, cap time.Duration, resetToWanted bool, now time.Time) error
```

- [ ] **Step 1: Failing test** for the math (exact expectations locked here):

```go
func TestNextBackoff(t *testing.T) {
	base, cap := 15*time.Minute, 24*time.Hour
	cases := []struct{ retries int; want time.Duration }{
		{1, 30 * time.Minute},   // 15m * 2^1
		{2, 1 * time.Hour},
		{3, 2 * time.Hour},
		{7, 24 * time.Hour},     // 15m*2^7=32h → capped
		{50, 24 * time.Hour},    // no overflow
	}
	...
}
```

Guard against `1<<retries` overflow: compute in float or clamp retries at 32.

- [ ] **Step 2:** Run — FAIL. **Step 3:** Implement. **Step 4:** Run — PASS.
- [ ] **Step 5:** Copy `engine/query.go` → `pipeline/query.go` and `engine/paths.go` → `pipeline/paths.go` changing only the package clause; copy their test files the same way. Run `go test ./internal/pipeline/`.
- [ ] **Step 6: Commit** `git commit -m "feat(pipeline): ports, exponential backoff helper, query/path helpers"`

---

### Task 5: Config — [pipeline] section

**Files:**
- Modify: `internal/config/config.go`
- Modify: `config.example.toml`
- Test: extend the existing config test file (find it: `ls internal/config/`)

**Interfaces:**
- Produces: `config.Pipeline` struct with fields (all `time.Duration` via the file's existing duration-parsing pattern — read how `[engine]` durations are parsed and copy it): `MaxActive int`, `MaxRetries int`, `BackoffBase`, `BackoffCap`, `CandidateTTL`, `FailedReviveAfter`, `StuckAfter`, `TickTimeout`, `WantedSyncInterval`, `DiscoveryInterval`, `SelectingInterval`, `DownloadingInterval`, `ImportingInterval`, `ImportConfirmTimeout` (default `"3m"`, consumed by Task 10's confirm phase) — defaults per Global Constraints; `MaxActive` default 30 (today's `max_concurrent_active`).

Keep `[engine]` untouched for now (legacy engine still reads it; deleted in Task 12). Matcher/transfer knobs the pipeline also needs (`search_timeout`, `transfer_deadline`, `stall_timeout`, `max_inflight_per_peer`, `max_transfer_retries`, `max_candidates_per_album`, `max_candidate_file_ratio`, `min_bitrate`, weights) are read from `[engine]` by both until Task 12 moves them to `[pipeline]` — one source of truth per key at all times.

- [ ] **Step 1:** Failing test: TOML snippet with `[pipeline]` overrides parses; empty `[pipeline]` yields all defaults.
- [ ] **Step 2-4:** Red → implement → green (`go test ./internal/config/`).
- [ ] **Step 5:** Add a commented `[pipeline]` block to `config.example.toml` documenting every key and default.
- [ ] **Step 6: Commit** `git commit -m "feat(config): pipeline section"`

---

### Task 6: WantedSync module

**Files:**
- Create: `internal/pipeline/wanted.go`
- Test: `internal/pipeline/wanted_test.go` (+ `internal/pipeline/pipeline_shared_test.go` for TestMain + fakes)

**Interfaces:**
- Consumes: `MusicSource.WantedMissing`, store: `UpsertWantedJob`, `UpdateJobMetadata`, `BackfillJobMetadataIfEmpty`, `CancelJobsNotWanted`, `ReviveFailedJobs`, `PruneJobEvents`, `AddJobEvent`.
- Produces: `type WantedSync struct`, `func NewWantedSync(p WantedSyncParams) *WantedSync`, satisfying `Module`. Also produces `func (w *WantedSync) Wanted() map[int64]lidarr.WantedAlbum` — a mutex-guarded snapshot of the last successful sync, consumed by Discovery (album metadata for the query) — **this is the one in-memory hand-off between modules and it is read-only advisory data, never state**.

Test setup file (`pipeline_shared_test.go`): copy the TestMain + `newBackedStore` pattern and the `fakeMusic`/`fakeSearcher` fakes from `engine/discovery_test.go:26-100` (trim to what pipeline needs as tasks progress).

Tick behaviour (port semantics from `engine/discovery.go:93-180` syncWanted/retryFailedJobs, adjusted):
1. `WantedMissing` → on error log+return (keep last snapshot).
2. For each wanted album: `UpsertWantedJob`; if returned job is in WANTED refresh metadata (`UpdateJobMetadata`), else `BackfillJobMetadataIfEmpty` — same updated_at-starvation rationale as the old comment block at `discovery.go:135-144`; port that comment.
3. `CancelJobsNotWanted(wantedIDs)` — record each cancellation with a new event type added in this task: `core.EventJobCancelled JobEventType = "job_cancelled"` (one-line addition to `core/state.go`; the dashboard renders event types generically — verify by reading `observ/events.go` before assuming, and add a label if it does not).
4. `ReviveFailedJobs(wantedIDs, now-FailedReviveAfter)` — log count when > 0.
5. `PruneJobEvents(now)`.
6. Store snapshot for `Wanted()`.

- [ ] **Step 1: Failing tests:**

```go
func TestWantedSyncUpsertsAndCancels(t *testing.T)
// fakeMusic with albums {1,2}; pre-existing job for album 3 in DOWNLOADING
// → jobs 1,2 exist in WANTED; job 3 CANCELLED

func TestWantedSyncRevivesOldFailed(t *testing.T)
// FAILED job (failed_at 31d ago) for a still-wanted album → WANTED, retries 0
// FAILED job for album absent from wanted list → untouched

func TestWantedSyncKeepsSnapshotOnLidarrError(t *testing.T)
// first tick ok → Wanted() has data; second tick Lidarr errors → Wanted() unchanged, no cancels ran
```

That third test pins the critical ordering: **cancellation must be skipped entirely when WantedMissing fails** — otherwise a Lidarr outage would cancel every job.

- [ ] **Steps 2-4:** Red → implement → green (`go test ./internal/pipeline/ -race`).
- [ ] **Step 5: Commit** `git commit -m "feat(pipeline): WantedSync module"`

---

### Task 7: Discovery module

**Files:**
- Create: `internal/pipeline/discovery.go`
- Test: `internal/pipeline/discovery_test.go`

**Interfaces:**
- Consumes: `PeerSearcher.Search`, `Ranker.Rank` (copy `Ranker` interface from `engine/ports.go:90-92`), `MusicSource.AlbumStatus`, WantedSync's `Wanted()`, store: `RunnableJobsInState`, `InsertCandidates`, `AdvanceJobStateFrom`, `ReliabilityFor`, `AddJobEvent`, plus `failOrBackoff`.
- Produces: `Discovery` module. Per tick it processes **one** WANTED job (the tick is 30s; one search per tick is deliberate pacing — port the batch=1 decision into the doc comment) — the highest-release-date runnable WANTED job.

Tick behaviour (ports `startJob`'s front half, `engine/discovery.go:222-323`, stopping before CreateAttempt):
1. `RunnableJobsInState(StateWanted, now, 1)`; empty → return nil.
2. Album metadata from `Wanted()[job.LidarrAlbumID]`; missing → skip (WantedSync will cancel it next sync; do NOT cancel here — Discovery never writes CANCELLED).
3. Search primary query; empty → normalized fallback query (port verbatim incl. `EventSearchFallback` event, `discovery.go:255-274`).
4. `ReliabilityFor` + `Rank` (port `uniqueUsernames`, `discovery.go:456-466`).
5. `AlbumStatus` for expected track count; apply the two skip-filters (file-ratio and fewer-files, `discovery.go:296-323`) **at cache time** — filtered candidates are never persisted; record `EventCandidateRejected` events as today.
6. Cap surviving candidates at `MaxCandidates` (today's `max_candidates_per_album`, highest score first).
7. Zero survivors → `failOrBackoff(resetToWanted=false)` + `EventSearch` event with the empty outcome.
8. Otherwise `InsertCandidates` then `AdvanceJobStateFrom(job.ID, StateWanted, StateSelecting)`; on false (cancelled underneath) delete nothing — candidates for a cancelled job are inert rows cleaned by the next ResetJobToWanted/never; acceptable. Reset retries to 0 as part of `InsertCandidates`' tx? **Decision: yes** — add `resetRetries` inside `InsertCandidates`' transaction (`UPDATE album_jobs SET retries=0, not_before=NULL WHERE id=$1`); document in the store method comment. (Adjust Task 2's method if implementing in order — this is the authoritative definition.)

- [ ] **Step 1: Failing tests** (fakes + backed store; port relevant scenarios from `engine/discovery_test.go` — grep it for `TestStartJob` / search-fallback / file-ratio tests and translate):

```go
func TestDiscoveryCachesRankedCandidates(t *testing.T)
// 3 results, one fails file-ratio filter → 2 candidates rows NEW, job SELECTING, retries reset to 0

func TestDiscoveryEmptySearchBacksOffExponentially(t *testing.T)
// empty results twice → retries 1 then 2; not_before ≈ now+30m then now+1h; job stays WANTED

func TestDiscoveryFailsJobAtMaxRetries(t *testing.T)
// job.Retries = maxRetries-1, empty search → state FAILED, failed_at set

func TestDiscoveryFallbackQuery(t *testing.T)
// primary empty, normalized query returns results → candidates cached, EventSearchFallback recorded

func TestDiscoveryOrdersByReleaseDateDesc(t *testing.T)
// two WANTED jobs → the newer release is searched first
```

- [ ] **Steps 2-4:** Red → implement → green.
- [ ] **Step 5: Commit** `git commit -m "feat(pipeline): Discovery module with candidate cache"`

---

### Task 8: Selecting module

**Files:**
- Create: `internal/pipeline/selecting.go`
- Test: `internal/pipeline/selecting_test.go`

**Interfaces:**
- Consumes: store: `RunnableJobsInState`, `NextNewCandidate`, `ActivateCandidate`, `ResetJobToWanted`, `RecordPendingTransfer`, `AddJobEvent`; `failOrBackoff`; **produces** `topUpCandidate(ctx, candidateID int64, now time.Time) (int, error)` as a shared unexported helper placed in `selecting.go` and reused by Downloading (port of `topUpAttempt`, `engine/discovery.go:370-418`, s/attempt/candidate/) — consumes `PeerSearcher.Enqueue`, store `TransfersForCandidate`, `RecordEnqueueIntent`, `RetryTransfer`, `UpdateTransferProgress`, `AttachTransferID`. Both modules embed a shared `deps` struct? **No — keep modules independent**: put `topUpCandidate` as a free function taking an explicit small interface `topUpDeps` declared beside it; both modules' param structs satisfy it.

Tick behaviour, for each of `RunnableJobsInState(StateSelecting, now, MaxActive)`:
1. `NextNewCandidate`; none left → `failOrBackoff(resetToWanted=true)` (deletes cache, retries++, → WANTED or FAILED).
2. TTL: `now.Sub(cand.CreatedAt) > CandidateTTL` → `ResetJobToWanted(job.Retries /*unchanged*/, nil, now)` + event detail "candidate cache expired, re-searching"; continue.
3. `ActivateCandidate(cand.ID, job.ID, MaxActive)`; false → continue (cap full or job cancelled — next tick retries).
4. Write-ahead every `cand.Files` entry via `RecordPendingTransfer(cand.ID, ...)`, then `topUpCandidate` — port the ordering and rationale comments from `startJob`'s back half (`discovery.go:328-351`) including `EventCandidateSelected`.

- [ ] **Step 1: Failing tests:**

```go
func TestSelectingActivatesBestCandidateAndEnqueues(t *testing.T)
// 2 NEW candidates → higher score ACTIVE, job DOWNLOADING, PENDING transfers for every file,
// MaxInflightPerPeer of them sent to fakeSearcher, EventCandidateSelected recorded

func TestSelectingRespectsMaxActive(t *testing.T)
// maxActive jobs already DOWNLOADING → job stays SELECTING, candidate stays NEW

func TestSelectingExhaustionBacksOffToWanted(t *testing.T)
// all candidates FAILED → candidates deleted, job WANTED, retries+1, not_before set

func TestSelectingExhaustionAtMaxRetriesFails(t *testing.T)

func TestSelectingExpiresStaleCache(t *testing.T)
// NEW candidate created 25h ago → job WANTED, retries UNCHANGED, candidates deleted
```

- [ ] **Steps 2-4:** Red → implement → green.
- [ ] **Step 5: Commit** `git commit -m "feat(pipeline): Selecting module with MaxActive gate and cache TTL"`

---

### Task 9: Downloading module (reconciler absorbed)

**Files:**
- Create: `internal/pipeline/downloading.go`
- Test: `internal/pipeline/downloading_test.go`

**Interfaces:**
- Consumes: `PeerNetwork` (ListDownloads/Cancel), `PeerSearcher` (Enqueue/Cancel/DeleteDownloadFolder), store: transfer methods (`ActiveTransfers`, `TransfersPastDeadline`, `UpdateTransferProgress`, `RetryTransfer`, `AttachTransferID`, `FindTransferByFallback` not needed — check), `RunnableJobsInState`, `ActiveCandidate`, `TransfersForCandidate`, `FailCandidate`, `AdvanceJobStateFrom`, `RecordAttemptOutcome`, `AddJobEvent`; `topUpCandidate` from Task 8; `commonLeaf`/`cleanup` from `paths.go`.
- Produces: `Downloading` module; `ReconcileStats` moves here (copy struct from `engine/reconciler.go:13-21`); a `MetricsSink` interface copied from `engine/engine.go:14-18` for observ wiring later.

Tick = three phases in order, each a direct port:
1. **Reconcile** — port `Reconciler.Reconcile` + `mapSlskdState` + `isRetryable` (`engine/reconciler.go:42-212`) as private methods. Behaviour byte-for-byte identical.
2. **Resolve** — for every job in `RunnableJobsInState(StateDownloading, now, MaxActive)`: port `resolveDownloadingJob` (`engine/discovery.go:568-669`) with these mappings and NOTHING else changed:
   - `AttemptsForJob` last-attempt dance → `ActiveCandidate(job.ID)`; none → skip.
   - `TransfersForAttempt` → `TransfersForCandidate`.
   - Success: `AdvanceJobStateFrom(job.ID, StateDownloading, StateImporting)` (VERIFYING no longer exists).
   - Fail (all terminal): `cleanupFolder` (the shared port of `cleanupAttempt`, `discovery.go:812-825`, placed in `paths.go` — see Task 10's Interfaces block for its home; create it here since Task 9 runs first), `RecordAttemptOutcome(false)`, `FailCandidate(cand.ID, "transfer failed")`, `AdvanceJobStateFrom(job.ID, StateDownloading, StateSelecting)` — **no cooldown, no retries bump** (per-candidate failure is free).
   - The two-phase cancel logic (pending siblings cancelled in DB, active siblings cancelled in slskd, `slskd.IsNotFound` treated as already-terminal, defer cleanup while any sibling live) is ported verbatim including all comments.
3. **Top-up** — port `topUpDownloads` (`discovery.go:427-451`) using `ActiveCandidate` + `topUpCandidate`.

No startup sweep exists or is needed: phase 1 then phase 2 in the same tick IS the sweep (spec: "Downloading's first tick is the old SweepStaleDownloads").

- [ ] **Step 1: Failing tests** — migrate the reconciler suite (`engine/reconciler_test.go`, all scenarios: adopt, complete, lost-with-retry, deadline-cancel, stall, retryable rejection, unknown count) and the downloading-resolution suite from `engine/discovery_test.go` (grep `TestAdvanceDownloading|TestResolve|TestSweep|two-phase|sibling`). Key new-model assertions:

```go
func TestDownloadingSuccessAdvancesToImporting(t *testing.T)
func TestDownloadingFailureReturnsJobToSelectingWithoutRetryBump(t *testing.T)
// candidate FAILED, job SELECTING, job.Retries unchanged, RecordAttemptOutcome(false) called
func TestDownloadingTwoPhaseFailWaitsForLiveSiblings(t *testing.T)
func TestDownloadingFirstTickResolvesStaleBacklog(t *testing.T)
// jobs whose transfers are already terminal in DB (crash legacy) resolve on tick one
```

- [ ] **Steps 2-4:** Red → implement → green (`-race`).
- [ ] **Step 5: Commit** `git commit -m "feat(pipeline): Downloading module absorbing reconciler"`

---

### Task 10: Importing module

**Files:**
- Create: `internal/pipeline/importing.go`
- Test: `internal/pipeline/importing_test.go`

**Interfaces:**
- Consumes: `MusicSource` (ManualImportCandidates/ExecuteManualImport/AlbumStatus), store: `RunnableJobsInState`, `ActiveCandidate`, `TransfersForCandidate`, `MarkImportSubmitted`, `SucceedCandidate`, `FailCandidate`, `AdvanceJobStateFrom`, `RecordAttemptOutcome`, `AddJobEvent`; `AlbumFolder` from paths.go; cleanup helper (share the ported `cleanupAttempt` — place it in `paths.go` next to `commonLeaf` as `func cleanupFolder(ctx, peers interface{ DeleteDownloadFolder(...) }, log, jobID, filenames)` so Downloading and Importing share one copy).

One IMPORTING state, two phases keyed on `candidate.ImportSubmittedAt`:
- **NULL → verify phase**: port `advanceImporting` (`discovery.go:677-774`) with mappings: rejection/incomplete → `FailCandidate` + `RecordAttemptOutcome(false)` + cleanup + `AdvanceJobStateFrom(IMPORTING→SELECTING)` (no cooldown); empty-folder-idempotency path → `SucceedCandidate` + `AdvanceJobStateFrom(IMPORTING→DONE)`; success → `ExecuteManualImport` then `MarkImportSubmitted` (crash between the two re-runs verify; the empty-folder path absorbs it — port that comment, `discovery.go:703-717`).
- **Set → confirm phase**: port `confirmImports` (`discovery.go:845-894`): `AlbumStatus`; complete → `SucceedCandidate` + `RecordAttemptOutcome(true)` + → DONE; overdue (`now.Sub(cand.ImportSubmittedAt) > ImportConfirmTimeout`... note: config key stays, moves to `[pipeline]` in Task 12 as `import_confirm_timeout = "3m"` — ADD to Task 5's struct now if executing in order; authoritative list includes it) → fail candidate → SELECTING.
- **Stuck escalation**: port `escalateIfStuck` (`discovery.go:783-798`) for persistently erroring Lidarr calls, threshold `StuckAfter`, verify phase only (confirm phase has its own timeout above).

- [ ] **Step 1: Failing tests** — migrate verify/confirm scenarios from `engine/discovery_test.go` (grep `TestAdvanceImporting|TestConfirm|rejrect|incomplete|escalate`):

```go
func TestImportingVerifyRejectionFailsCandidateToSelecting(t *testing.T)
func TestImportingIncompleteCoverageFailsCandidate(t *testing.T)
func TestImportingHappyPathSubmitsThenConfirmsToDone(t *testing.T)
// tick 1: ExecuteManualImport called, ImportSubmittedAt set, still IMPORTING
// tick 2 (albumStatus complete): candidate SUCCEEDED, RecordAttemptOutcome(true), job DONE
func TestImportingConfirmTimeoutFailsCandidate(t *testing.T)
func TestImportingEmptyFolderIdempotentDone(t *testing.T)
func TestImportingStuckEscalation(t *testing.T)
```

- [ ] **Steps 2-4:** Red → implement → green.
- [ ] **Step 5: Commit** `git commit -m "feat(pipeline): Importing module (verify gate + async confirm)"`

---

### Task 11: Full-lifecycle integration test

**Files:**
- Test: `internal/pipeline/integration_test.go`

- [ ] **Step 1:** Write the test: real store, all five modules constructed with shared fakes; drive by calling `Tick` in a loop (not the Runner — determinism) with a stepped fake clock:

```go
func TestFullLifecycleWantedToDone(t *testing.T)
// seed fakeMusic wanted list → WantedSync.Tick → job WANTED
// → Discovery.Tick → SELECTING with candidates
// → Selecting.Tick → DOWNLOADING, transfers PENDING/QUEUED
// → fakeNetwork completes transfers → Downloading.Tick → IMPORTING
// → Importing.Tick ×2 (submit, confirm) → DONE, candidate SUCCEEDED,
//   RecordAttemptOutcome success visible in known_users

func TestFullLifecycleFailedCandidateRotation(t *testing.T)
// first candidate's transfers error → back to SELECTING → second candidate downloads → DONE

func TestFullLifecycleExhaustionToFailedAndRevival(t *testing.T)
// empty searches until max_retries → FAILED; advance clock 31d → WantedSync.Tick → WANTED
```

- [ ] **Step 2:** Run — these should pass immediately if Tasks 6-10 are correct; any failure here is a real integration bug: debug with superpowers:systematic-debugging, do not weaken assertions.
- [ ] **Step 3: Commit** `git commit -m "test(pipeline): full lifecycle integration tests"`

---

### Task 12: Swap main.go to pipeline, delete engine, finalize schema + config

**Files:**
- Modify: `cmd/slusk/main.go` (read it fully first)
- Modify: `internal/config/config.go`, `config.example.toml` (move matcher/transfer keys `[engine]`→`[pipeline]`, delete `[engine]`)
- Modify: `internal/store/schema.sql` + store SQL (column rename, FK, drop candidate_attempts)
- Delete: `internal/engine/` (entire package)
- Modify: `internal/observ/status.go` (+ whatever references engine — grep `"internal/engine"` across the repo first)

- [ ] **Step 1:** Grep consumers: `grep -rn "internal/engine" --include="*.go" | grep -v engine/`. Expect `main.go` and possibly `observ`. Plan the wiring: main constructs store/clients/matcher as today, then the five modules + `pipeline.NewRunner`, replacing `engine.New(...).Run(ctx)` with `runner.Run(ctx)`.
- [ ] **Step 2:** observ health: replace the `engine.Healthy(staleAfter)`-based check (find it in `observ/status.go` / `main.go` wiring) with `runner.Healthy()`; expose `runner.Health()` in the `/status` payload as `modules: {name: lastTick}`.
- [ ] **Step 2b:** Metrics (spec's observability section): keep the existing `MetricsSink` gauges fed from Downloading's reconcile stats, and add two cheap ones fed by the runner: a per-module tick counter (`pipeline_ticks_total{module}`) and per-module lastTick gauge. `pipeline_jobs{state}` is served by a small `CountJobsInStates`-based gauge refreshed in WantedSync's tick (it already touches every state). Skip `candidates_cached` and tick-duration histograms unless trivially available — YAGNI over the spec's suggestion list.
- [ ] **Step 3:** Schema finalization in `schema.sql`:

```sql
-- transfers now references candidates; renamed from the legacy attempt_id.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='transfers' AND column_name='attempt_id') THEN
        ALTER TABLE transfers RENAME COLUMN attempt_id TO candidate_id;
    END IF;
END $$;
ALTER TABLE transfers ADD CONSTRAINT transfers_candidate_id_fkey
    FOREIGN KEY (candidate_id) REFERENCES candidates(id) NOT VALID; -- NOT VALID: pre-existing rows are wiped by the clean-slate script anyway
DROP TABLE IF EXISTS candidate_attempts;
```

Guard the ADD CONSTRAINT with an existence check like the rename (constraint adds are not IF NOT EXISTS-able). Update every store query that says `attempt_id` to `candidate_id`. Remove `candidates_tried`/`next_attempt_at` from Go code paths but LEAVE the columns (spec: dropped after rollout is verified).
- [ ] **Step 4:** Delete `internal/engine/`. Before deleting, run `grep -rn "TestSweep\|escalate\|two-phase" internal/pipeline/` — sanity that the ported test scenarios exist. Move `mapstate_test.go`-covered logic if it tests `mapSlskdState` (now in downloading.go) — verify the pipeline copy has equivalent coverage first.
- [ ] **Step 5:** `go build ./... && go test ./... -count=1` — full green.
- [ ] **Step 6:** Update `docker-compose`/docs references to `[engine]` config keys: `grep -rn "max_concurrent_active\|tick_interval\|candidate_backoff\|failed_retry_after" --include="*.toml" --include="*.md" .`
- [ ] **Step 7: Commit** `git commit -m "feat!: replace engine with pipeline state machine"` (breaking: config section rename, requires clean-slate deploy per scripts/clean-slate-pipeline.sh)

---

### Task 13: Dashboard adaptation

**Files:**
- Modify: `internal/store/dashboard.go` (JobView/JobDetail queries: candidate_attempts → candidates)
- Modify: `internal/core/models.go` (JobView/AttemptDetail/JobDetail reshaped around Candidate)
- Modify: `internal/observ/` (state names/colors, retries + not_before display, per-module health, retry endpoint)
- Test: `internal/store/dashboard_test.go`, `internal/observ/` tests (read existing test style first)

- [ ] **Step 1:** Read `internal/observ/web.go`, `jobdetail.go`, `status.go` and `store/dashboard.go` in full before touching anything — the UI templates enumerate state names.
- [ ] **Step 2:** Store: rewrite `jobViewSelect`/`JobDetail` against `candidates` (latest candidate per job = `ORDER BY created_at DESC LIMIT 1`, same shape as the old attempt subqueries; keep the covering-index rationale — add `idx_candidates_job_created ON candidates(album_job_id, created_at)` if the old idx_attempts_job pattern is being replicated). Failing tests first in `dashboard_test.go`.
- [ ] **Step 3:** New endpoint `POST /api/jobs/{id}/retry`: store method

```go
// RetryFailedJob manually revives one FAILED job: retries 0, not_before/failed_at
// cleared, candidates deleted, state WANTED. Returns false when the job is not
// FAILED (the dashboard button raced a state change).
func (s *Store) RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error)
```

  TDD it in `dashboard_test.go`; handler returns 409 on false, 204 on true; add the button in the job detail panel for FAILED jobs only.
- [ ] **Step 4:** Job list/detail UI: render the 7 new state names (Swedish labels consistent with existing UI copy — read the current label map), show `retries` and a "sover till HH:MM" badge when `not_before` is future, per-module health on the status view from `/status`'s new `modules` map.
- [ ] **Step 5:** `go test ./... -count=1` full green; then run the app against the dev docker-compose and eyeball the dashboard (see `/docs/smoke-test.md` if it exists — it does: follow it).
- [ ] **Step 6: Commit** `git commit -m "feat(observ): dashboard for pipeline states, per-module health, manual retry"`

---

### Task 14: Docs + deploy notes

**Files:**
- Modify: `docs/smoke-test.md` (new state names/flow), `config.example.toml` final pass, `README`/`CLAUDE.md` if they describe the engine (grep for "engine", "COOLDOWN", "reconcil").

- [ ] **Step 1:** Grep + update stale references. Verify `scripts/clean-slate-pipeline.sh` table list still matches reality (it wipes `candidates` too — it does, it was written table-tolerant).
- [ ] **Step 2:** `go vet ./... && go build ./... && go test ./... -count=1` — final full green.
- [ ] **Step 3: Commit** `git commit -m "docs: pipeline rewrite deploy and smoke-test updates"`
- [ ] **Step 4:** Use superpowers:finishing-a-development-branch — PR onto main with body referencing the spec and the clean-slate deploy procedure (stop container → run `scripts/clean-slate-pipeline.sh "$DSN"` → clear slskd downloads dir manually → start new tag).

---

## Execution notes

- Tasks 6-10 each port hard-won logic. The rule is: **port comments along with code** — the doc comments in `discovery.go`/`reconciler.go` encode incident history (queued-megabyte limits, slskd restarts, cross-peer folder collisions). A port that drops the comment loses the why.
- When a Task 2 signature turns out to need adjustment during Tasks 6-10 (it will — e.g. `InsertCandidates` resetting retries is already flagged in Task 7), adjust the store method AND its test in the same commit as the consumer; do not fork a second method.
- `go test ./internal/pipeline/ -race` on every module task, not just the runner.
