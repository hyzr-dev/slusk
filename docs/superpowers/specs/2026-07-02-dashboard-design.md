# slusk Dashboard — Design

## Context

slusk is currently a headless daemon: `cmd/slusk/main.go` wires config, store, engine, and an `observ.Server` that exposes `/metrics` (Prometheus) and `/status` (a small JSON summary). There is no HTTP UI at all.

A Claude Design mockup, `Slusk Dashboard.dc.html` (imported from `claude.ai/design/p/9085f510-06a3-4d25-b01d-f992601dd938`), specifies a 4-view dark-themed dashboard: **Översikt** (overview stats + active downloads + recent reconcile passes), **Kö** (queue table with expandable rows: peer/transfer info, files, history, retry/force-search/cancel/remove actions), **Hälsa** (health metric tiles + sparkline/line charts), **Inställningar** (Lidarr/slskd connection + engine settings editor). The mockup uses fake in-browser simulated data (`setInterval` ticking progress) in a Claude-internal "DC" component format — it exists purely as a visual/interaction reference, not code to port directly.

This design covers a first real implementation: **Översikt + Kö only**, backed by live data from `internal/store`, served from the existing `observ.Server`.

## Scope

**In scope (v1):**
- New `GET /` HTML page on the existing observability HTTP server, with two views (Overview, Queue) toggled client-side
- New `GET /api/jobs` JSON endpoint listing every album job joined with its most recent transfer
- New `POST /api/jobs/{id}/cancel` endpoint wired to the existing `slskd.Client.Cancel` and `store.AdvanceJobState`
- Schema migration: cache `title`/`artist_name` on `album_jobs` at discovery time, so the UI doesn't need a live Lidarr call per render
- Visual styling ported from the mockup's CSS (dark theme, IBM Plex fonts, color tokens)

**Out of scope (future issues):**
- Hälsa (health charts) view — needs a metrics time-series store the project doesn't have yet
- Inställningar (settings editor) view — needs a config-writeback mechanism (currently config is a static TOML file loaded once at startup)
- Expandable row's "Filer" (per-track file status) and "Historik" (event timeline) — needs an event log the store doesn't have yet; only the "Peer & transfer" block (data already available) ships in v1
- Retry / Tvinga sökning (force search) row actions — only **Avbryt** (Cancel) ships in v1; the others need new engine-level logic (retry-to-DISCOVERED, forced re-search) better scoped as their own issues
- Per-row "orphaned" detail (peer/progress/reason for slskd transfers with no matching Lidarr job) — the reconciler only counts these today (`stats.Unknown`), it doesn't persist per-item detail; only the existing aggregate count (via `StatusReport.Orphaned`) is available for the Overview stat card
- Any JS build step, bundler, or frontend framework — vanilla JS + Go `html/template`, no new dependencies

## Architecture

```
main.go
  └─ observ.NewServer(reg, statusFn, jobsFn, cancelFn)
       ├─ GET  /metrics        (unchanged)
       ├─ GET  /status         (unchanged)
       ├─ GET  /               → dashboard.html (embedded template)
       ├─ GET  /dashboard.js   → embedded static JS asset
       ├─ GET  /api/jobs       → jobsFn(ctx) → []core.JobView as JSON
       └─ POST /api/jobs/{id}/cancel → cancelFn(ctx, id) → 204 / 404 / 502
```

`internal/observ` keeps its existing rule of not importing `store` or `engine` directly. Like today's `StatusFunc`, the new endpoints take plain function types (`JobsFunc`, `CancelFunc`) that `main.go` closes over `st` (the store) and `peers` (the slskd client) to implement — `observ` stays a leaf package.

## Data model changes

`internal/store/schema.sql`: add two columns to `album_jobs`:

```sql
ALTER TABLE album_jobs ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE album_jobs ADD COLUMN artist_name TEXT NOT NULL DEFAULT '';
```

`core.AlbumJob` gets matching `Title` and `ArtistName` fields.

`Discoverer.syncWanted` (`internal/engine/discovery.go:79`) already has `lidarr.WantedAlbum{Title, ArtistName}` for every album it upserts — `UpsertDiscoveredJob` is extended to accept and persist both fields instead of just `lidarr_album_id`.

## New store method

```go
// core.JobView is a read-only projection joining an AlbumJob with its most
// recent transfer, for display purposes only.
type JobView struct {
    Job      AlbumJob
    Transfer *Transfer // nil if the job has no attempt/transfer yet (e.g. still DISCOVERED)
    Peer     string    // Transfer.Username, surfaced separately for template convenience
}
```

```go
// ListJobsWithTransfer returns every non-cancelled album job joined with its
// most recent transfer (by candidate_attempts → transfers), newest first.
func (s *Store) ListJobsWithTransfer(ctx context.Context) ([]core.JobView, error)
```

Implementation: one query joining `album_jobs` → latest `candidate_attempts` row per job (`MAX(created_at)`) → its `transfers` row, left-joined so jobs with no attempt yet still appear with `Transfer: nil`. CANCELLED jobs are excluded (they're done, no reason to show them in an active queue view — matches the mockup's status set of active/stalled/queued/done).

**Orphaned transfers are not representable in v1.** The mockup shows "orphaned" as full table rows (peer, progress, reason), but that data doesn't exist: `Reconciler` only increments a counter (`stats.Unknown`, `reconciler.go:144`) for slskd transfers with no matching `album_jobs` row — nothing about them is persisted. `/api/jobs` therefore only ever returns job-backed rows (queued/active/stalled/done). The Overview stat card for "Orphaned" still works today via the existing `StatusReport.Orphaned` count; a per-item orphaned row in the Kö table would need the reconciler to persist unknown-transfer details first, which is out of scope for this design.

## Endpoints

**`GET /api/jobs`** — calls `ListJobsWithTransfer`, maps to a JSON array. Each entry carries what the Kö table needs: id, title, artist, state (mapped to the mockup's status vocabulary: queued/active/stalled/orphaned/done), peer, bytes done/total, updated_at. 500 + `{"error": "..."}` on store failure, matching the existing `/status` handler's error style (`observ.go:66-69`).

**`POST /api/jobs/{id}/cancel`**:
1. Look up the job's current transfer via store; 404 if the job id doesn't exist.
2. If a transfer with a non-empty `SlskdID` exists, call `slskd.Client.Cancel(ctx, username, slskdID)`. Log and continue past errors here (slskd may have already dropped it) rather than blocking the state transition — a stale slskd-side entry gets cleaned up by the next reconcile pass regardless.
3. `store.AdvanceJobState(ctx, jobID, core.StateCancelled, now)`.
4. Respond 204. If step 3 fails, respond 502.

## Frontend

- `internal/observ/web/dashboard.html` — Go `html/template`, embedded via `//go:embed`. Renders the page shell (sidebar nav, header) once; Overview/Queue view bodies are plain `<div>`s toggled by a `data-view` attribute and a few lines of vanilla JS (no `sc-if`/`sc-for` — those are mockup-only constructs).
- `internal/observ/web/dashboard.js` — embedded static asset. Polls `GET /api/jobs` every 3s, re-renders the stat cards and the queue table rows from the JSON response, handles search/status-filter client-side over the already-fetched data, expand/collapse row toggling, and wires the Cancel button to `POST /api/jobs/{id}/cancel` followed by an immediate re-poll.
- CSS is inlined in the template `<style>` block, copied from the mockup (dark palette, IBM Plex Sans/Mono, card/table styles) with the fake-data-only elements (sparkline SVGs, chart placeholders, settings form, reconcile countdown ticker) removed.

## Error handling

- Store/query errors on `/api/jobs`: 500, logged server-side, JSON `{"error": "..."}` body — page keeps showing last-successful data (JS skips the render if fetch fails, so a transient blip doesn't blank the table).
- Cancel errors: distinguished 404 (unknown job) vs 502 (state-transition failure) so the frontend can show "Job not found" vs "Cancel failed, try again" distinctly; slskd-side cancel failures are swallowed (see above) and not surfaced as an error to the user, since the state transition is the operation that matters.
- No auth/CSRF on `/api/jobs*` — matches the existing `/metrics`/`/status` posture (this server is meant to sit behind the user's own network boundary, not be internet-facing); out of scope to change here.

## Testing

- `internal/store`: table test for `ListJobsWithTransfer` covering a job with no attempt yet, a job with an in-progress transfer, and a job with a completed transfer — asserting the join picks the *latest* attempt when a job has retried candidates.
- `internal/observ`: `httptest`-based handler tests for `/api/jobs` (happy path + store-error path) and `/api/jobs/{id}/cancel` (happy path, 404, slskd-cancel-error-still-advances-state, store-error-502), following the existing pattern in `observ_test.go`.
- No automated JS tests (matches the project's current no-build-step approach) — manual verification: start the daemon against a seeded dev store, load `/`, confirm stat cards and queue rows render, expand a row, cancel a job, confirm it drops out of the active list on the next poll.
