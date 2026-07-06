# Pipeline state machine — design

**Date:** 2026-07-06
**Status:** Approved for planning
**Replaces:** the `internal/engine` package (engine.go, discovery.go, reconciler.go)

## Motivation

Three goals, in order:

1. **Robustness/isolation** — today everything runs in one goroutine with a
   single select loop; one hung stage freezes the whole application. The last
   three bug fixes (#39, #40, downloading-batch-window-deadlock) were all
   variations of "the loop wedged".
2. **Code structure** — `discovery.go` is ~900 lines and owns the entire
   pipeline. One module per state with a clear contract makes each piece
   independently understandable and testable.
3. **Simpler mental model** — fewer states, one owner per state, backoff as
   data instead of control flow.

Explicitly in scope as a behaviour change: a **persistent candidate cache**
(search once, score once, consume candidates one by one across failures).

## States

```
                  ┌───────────────────────────────────┐
                  │  candidates exhausted / empty     │
                  │  search / stale cache             │
                  ▼  (retries++ except stale cache)   │
Lidarr ──► WANTED ──► SELECTING ──► DOWNLOADING ──► IMPORTING ──► DONE
 (sync)      │    search   │   ▲          │             │
             │             │   └──────────┴─────────────┘
             │             │      candidate failed → next candidate
             ├── CANCELLED (album left the wanted list — terminal)
             └── FAILED    (retries == max_retries — terminal, revivable)
```

Seven states, three end states (DONE and CANCELLED terminal, FAILED revivable). State = which queue the job sits in. Exactly
one module writes transitions *out of* each non-terminal state.

| State | Owner module | Meaning |
|---|---|---|
| WANTED | Discovery | Synced from Lidarr, awaiting search |
| SELECTING | Selecting | Has a cached, scored candidate list |
| DOWNLOADING | Downloading | Active candidate's transfers in flight in slskd |
| IMPORTING | Importing | Verify gate + Lidarr import + confirmation |
| DONE | — | Imported and confirmed |
| CANCELLED | — | Album left Lidarr's wanted list |
| FAILED | — (WantedSync revives) | Retry budget exhausted |

## Backoff and retries

Two columns on `album_jobs`, no wait states:

- `not_before TIMESTAMPTZ NULL` — every module filters
  `WHERE not_before IS NULL OR not_before <= now()`. Sleeping jobs are simply
  invisible; nothing has to "wake" them.
- `retries BIGINT DEFAULT 0` — incremented only on **search-cycle failures**
  (empty search result, or all cached candidates exhausted). Then
  `not_before = now + base * 2^retries`, base 15 min, capped at 24 h.
- Per-candidate failures (download or import) do **not** touch `retries` —
  the next cached candidate is free and immediate.
- `retries` resets to 0 when a search yields candidates (a fresh cycle).
- `retries == max_retries` (config, default 10 ≈ 4–5 days of attempts) →
  **FAILED**, `failed_at = now`.

**Revival from FAILED:**

1. Manual: dashboard "retry" button resets `retries`/`not_before`/`failed_at`
   → WANTED.
2. Automatic: WantedSync re-queues a FAILED job still present in Lidarr's
   wanted list once `failed_at` is older than 30 days (config).

COOLDOWN and non-terminal FAILED from the old model disappear entirely.

## Modules and runner

`internal/pipeline/` — one file per module plus a deliberately minimal shared
runner:

```go
type Module interface {
    Name() string
    Tick(ctx context.Context, now time.Time) error
}
```

The runner starts one goroutine per module: own ticker, per-tick
`context.WithTimeout` (default 5 min, the lesson from #39 carried over),
panic recovery, per-module `lastTick` for health.

| Module | Default tick | Per tick |
|---|---|---|
| WantedSync | 15 min | Lidarr poll → upsert WANTED; CANCELLED for removed albums (from *any* state); 30-day FAILED revival; job_events pruning |
| Discovery | 30 s | Oldest runnable WANTED in `release_date DESC` order → slskd search → score via matcher → persist candidates → SELECTING. Empty search: `retries++`, stay WANTED |
| Selecting | 10 s | Pick best NEW candidate → mark ACTIVE → write-ahead PENDING transfers → DOWNLOADING. No NEW left: delete candidates, `retries++` → WANTED. Cache older than `candidate_ttl` (24 h): delete candidates → WANTED *without* `retries++` |
| Downloading | 15 s | Poll slskd transfers (absorbs the old Reconciler: adoption, stall detection, unknown-transfer tracking), top-up PENDING within MaxInflightPerPeer, two-phase fail cleanup. All complete → IMPORTING; candidate failed → FAILED candidate → SELECTING |
| Importing | 30 s | ManualImportCandidates verify gate (any rejection or incomplete coverage → candidate FAILED → SELECTING), trigger import, confirm via AlbumStatus → DONE |

Selecting ticks fastest on purpose: it is the pipeline's valve — its latency
determines how quickly a freed MaxActive slot is refilled.

### Concurrency invariants

1. **One goroutine per state, one state per goroutine.** No row locking
   needed. Transitions are still written as
   `UPDATE ... WHERE id = ? AND state = ?` (belt and braces; makes a buggy
   double-run harmless).
2. **MaxActive is owned by Selecting alone**: "max N jobs in
   DOWNLOADING + IMPORTING", checked in the same transaction that activates a
   candidate. Discovery is uncapped — searching and caching is cheap, and it
   keeps a warm candidate queue ready.
3. **One exception to rule 1:** WantedSync writes CANCELLED from any state
   when an album leaves the wanted list. Safe because it only ever takes rows
   *from* other modules; the conditional UPDATE in (1) makes every module
   tolerant of a job being cancelled under it (the transition bounces, move
   on).

## Schema

### `album_jobs`

```sql
ALTER TABLE album_jobs ADD COLUMN retries    BIGINT NOT NULL DEFAULT 0;
ALTER TABLE album_jobs ADD COLUMN not_before TIMESTAMPTZ;
ALTER TABLE album_jobs ADD COLUMN failed_at  TIMESTAMPTZ;
-- candidates_tried and next_attempt_at become unused (dropped after rollout)
```

### `candidates` (replaces `candidate_attempts`)

A candidate *is* its own attempt: `NEW` (cached, untried) → `ACTIVE` (picked
by Selecting) → `SUCCEEDED` | `FAILED`.

```sql
CREATE TABLE candidates (
    id            BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    album_job_id  BIGINT NOT NULL REFERENCES album_jobs(id),
    username      TEXT NOT NULL,
    score         DOUBLE PRECISION NOT NULL,
    files         JSONB NOT NULL,          -- [{filename, size}] cached from the search
    state         TEXT NOT NULL,           -- NEW | ACTIVE | SUCCEEDED | FAILED
    fail_reason   TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_candidates_job ON candidates(album_job_id, state);
```

`files` is JSONB by design (YAGNI): always read as one blob at enqueue time,
never row-by-row; `transfers` takes over as the source of truth the moment
the candidate goes ACTIVE. `transfers.attempt_id` is renamed
`candidate_id`.

When a selection cycle is exhausted the job's candidates are deleted (like
today's `ResetJobForRetry`). **Invariant preserved verbatim:** peer history in
`known_users` / `artist_user_reliability` is written incrementally at outcome
time and never derived from candidates, so it survives cache deletion.

### Unchanged

`transfers` (except the FK rename), `known_users`,
`artist_user_reliability`, `job_events`.

### State migration (one-off, idempotent, runs at startup like schema apply)

| Old | New | Notes |
|---|---|---|
| DISCOVERED, SEARCHING, SELECTING | WANTED | No cache exists → re-search |
| COOLDOWN | WANTED | `not_before = next_attempt_at` |
| DOWNLOADING | DOWNLOADING | Active attempt becomes an ACTIVE candidate; `files` rebuilt from its transfers |
| VERIFYING, IMPORTING | IMPORTING | |
| COMPLETED | DONE | |
| FAILED | FAILED | `failed_at = updated_at`, `retries = max_retries` |
| CANCELLED | CANCELLED | |

Old `candidate_attempts` rows for non-DOWNLOADING jobs are not migrated
(their jobs restart from WANTED anyway); peer reliability history already
lives in its own tables.

## Error handling

Default: a failed tick is logged, the job is left untouched, the next tick
retries. Three explicit mechanisms on top:

1. **Stuck-job escalation** (today's `escalateIfStuck`, rehomed): applies to
   IMPORTING only — a job whose Lidarr calls keep erroring for longer than
   `stuck_after` (default 1 h without an `updated_at` change) has its
   candidate failed → SELECTING. The other states need no escalation:
   WANTED and SELECTING are queues where waiting is legitimate (backlog,
   MaxActive full) and SELECTING staleness is already handled by
   `candidate_ttl`; DOWNLOADING has per-transfer stall detection and
   deadlines.
2. **Two-phase fail cleanup in Downloading** (cancel live siblings first,
   clean the folder only when everything is terminal) is ported verbatim
   with its tests — hard-won logic, do not redesign.
3. **Crash mid-tick.** All transitions stay idempotent and write-ahead
   (candidates persisted before enqueue, transfers PENDING before slskd is
   asked). No startup sweep special-case remains: Downloading's first tick
   *is* the old `SweepStaleDownloads`, because reconcile and advancement now
   live in the same module.

## Observability

- `/healthz`: per-module `lastTick`; unhealthy if any module hasn't ticked
  within `tick interval × 3 + tick timeout`. Stricter than today's single
  `lastReconcile`.
- Metrics: existing counters gain a `module` label; new
  `pipeline_tick_duration{module}`, `pipeline_jobs{state}`,
  `candidates_cached`.
- `job_events` unchanged (already state-agnostic). Pruning moves into
  WantedSync's tick.
- Dashboard (`observ`): map new state names, show `retries`/`not_before`,
  add a retry button for FAILED, per-module health.

## Testing

- Each module tested in isolation against the real Postgres test container
  (as store tests today) with fake slskd/lidarr clients behind the existing
  `ports.go` interfaces.
- The existing ~2,600 lines of engine tests migrate per module — the
  scenarios (wedge, stall, partial import, cross-peer collision, …) remain
  valid specifications, just rehomed.
- New integration test: all five modules against one DB, one job driven
  through the full WANTED→DONE lifecycle.

## Delivery

One PR, one commit per step, every commit builds with green tests:

1. Schema + store: new columns, `candidates` table, migration, store methods
2. Runner skeleton + WantedSync
3. Discovery + Selecting (the candidate cache)
4. Downloading (reconciler absorbed)
5. Importing
6. Swap `main.go` to pipeline, delete `internal/engine`
7. Dashboard adaptation

No legacy/config flag: rollback is running the previous Docker tag, which
works because the schema migration is additive (old columns stay until the
rollout is verified; the old engine ignores the new ones).

## Config

New/changed keys (names indicative):

```toml
[pipeline]
max_active            = 3        # unchanged semantics, now enforced by Selecting
max_retries           = 10
backoff_base          = "15m"
backoff_cap           = "24h"
candidate_ttl         = "24h"
failed_revive_after   = "720h"   # 30 days
stuck_after           = "1h"
tick_timeout          = "5m"
wanted_sync_interval  = "15m"
discovery_interval    = "30s"
selecting_interval    = "10s"
downloading_interval  = "15s"
importing_interval    = "30s"
```

Existing keys that map onto these (poll intervals, cooldown/failed-retry
durations) are renamed/removed in step 6.
