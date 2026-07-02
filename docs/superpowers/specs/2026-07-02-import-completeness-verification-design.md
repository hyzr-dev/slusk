# Import completeness verification

**Date:** 2026-07-02
**Status:** Approved (design), pending implementation plan

## Problem

The current import step assumes a finished slskd download equals a successful
Lidarr import. In `advanceImporting` (`internal/engine/discovery.go:308`) a job in
`VERIFYING` asks Lidarr for manual-import candidates, checks per-file rejections,
fires the async `ManualImport` command, and immediately marks the job
`COMPLETED` on an HTTP 2xx. Two gaps follow from this:

1. **No confirmation the import landed.** `ManualImport` is an asynchronous Lidarr
   command. We fire it and mark `COMPLETED` without confirming the command
   finished or that files reached the library. An async failure goes unnoticed.
2. **Completeness is never checked.** If a download covers only 8 of 12 tracks,
   the 8 valid files import without rejection and the job is marked `COMPLETED`,
   leaving the album incomplete in Lidarr.

## Goal

A job is only `COMPLETED` when the release is actually **complete in Lidarr**
(all expected tracks present). If the download cannot make the release complete,
or the import does not land, the attempt fails and the job rotates to the next
candidate/source using the existing cooldown machinery.

## Decisions

- **Definition of success:** the release is complete in Lidarr
  (`trackFileCount >= trackCount` for the album's monitored release).
- **No partial import:** if a download does not cover the full release, do not
  import any of it — fail the attempt and rotate to the next source. Keeps the
  library free of half-imported albums.
- **Confirmation mechanism:** watch the album's completeness directly (poll
  `GET /api/v1/album/{id}` across ticks), not the Lidarr command status. The end
  state is what we care about; measuring it directly is simpler and resumable.

## Design

### State machine

`advanceImporting` splits its logic across two job states. The `IMPORTING` state
already exists in `core.AlbumJobState` but is currently unused by the flow.

**VERIFYING — gate: decide whether to import at all**

1. Build the album folder from transfer filenames (`AlbumFolder`, existing).
2. `ManualImportCandidates(folder)`.
   - Empty list → files already imported (crash-idempotency shortcut); keep the
     existing behaviour: `SucceedAttempt` + `AdvanceJobState(COMPLETED)`.
3. If any candidate has a rejection → `FailAttempt("import rejected")` +
   `SetJobCooldown(FailedCandidateBackoff)` → rotate. (Existing behaviour.)
4. **Coverage gate (new):** fetch the album's total track count via
   `AlbumStatus`. Count distinct track IDs across the importable candidates. If
   `coverage < total` → `FailAttempt("incomplete download")` +
   `SetJobCooldown(FailedCandidateBackoff)` → rotate. **No import is executed.**
5. Otherwise: `ExecuteManualImport(importable)` then
   `AdvanceJobState(IMPORTING)`. Do **not** mark the attempt succeeded and do
   **not** go to `COMPLETED` yet.

**IMPORTING — confirm Lidarr is satisfied**

- `AlbumStatus(albumID)`:
  - `trackFileCount >= trackCount` → `SucceedAttempt` + `AdvanceJobState(COMPLETED)`.
  - Else if `now > job.UpdatedAt + ImportConfirmTimeout` →
    `FailAttempt("import not confirmed")` + `SetJobCooldown(FailedCandidateBackoff)`
    → rotate.
  - Else leave the job in `IMPORTING`; re-check on the next tick.

`job.UpdatedAt` is set by `AdvanceJobState` when the job enters `IMPORTING`, and
nothing else writes the job while it is in that state, so it serves as the
"entered IMPORTING at" timestamp for the timeout. No new schema column is
required.

The `Advance` dispatch must process both `VERIFYING` and `IMPORTING` jobs
through `advanceImporting` (today it fetches only `VERIFYING`).

### Lidarr client (`internal/lidarr/client.go`)

- **New** `AlbumStatus(ctx, albumID int64) (present, total int, err error)` —
  `GET /api/v1/album/{id}`, reading `statistics.trackFileCount` and
  `statistics.trackCount` (same `statistics` shape already parsed for
  wanted/missing).
- **Extend** `ManualImportItem` with `TrackIDs []int64`; parse `tracks[].id`
  from the manual-import response (currently discarded). Used to compute
  distinct-track coverage in the gate.
- **Interface:** add `AlbumStatus` to `MusicSource` in
  `internal/engine/ports.go`.

### Config

- Add `ImportConfirmTimeout time.Duration` to `DiscovererParams` and wire it from
  the existing config source. Default ~3 minutes — generous enough for Lidarr to
  process the import command before we treat it as failed.

## Coverage-gate semantics

Coverage is `len(distinct importable track IDs) >= total track count`. For a
fresh missing album (`trackFileCount == 0`) this is exactly "this source has the
whole release." For an album that is already partially present, the gate is
conservative: it requires a source carrying the full release rather than one
that only supplies the remaining tracks. This is a deliberate choice consistent
with "we want one complete release," and it keeps the gate logic simple (no need
to diff already-present track IDs against candidate track IDs).

## Known limitations (deliberately not built — YAGNI)

- **Partial pollution on async failure.** If the gate predicts completeness but
  Lidarr's async import drops a single file, the album lands at e.g. 11/12 and we
  rotate — leaving 11 tracks in the library. Rotation to a full copy re-imports /
  upgrades them. We do not build cleanup of partially imported files now.
- **Full-release requirement.** A source that only fills the last missing tracks
  of a partially present album is rejected by the gate. Accepted trade-off.

## Testing

- **Gate — incomplete download:** candidates cover fewer tracks than
  `AlbumStatus` total → attempt failed with "incomplete download", cooldown set,
  `ExecuteManualImport` not called, no `COMPLETED`.
- **Gate — rejection:** unchanged behaviour (rejected candidate → cooldown).
- **Gate — full coverage:** import executed, job moves to `IMPORTING`, attempt
  not yet succeeded.
- **Confirm — becomes complete:** `IMPORTING` job whose `AlbumStatus` reaches
  `trackFileCount >= trackCount` → `SucceedAttempt` + `COMPLETED`.
- **Confirm — timeout:** `IMPORTING` job still incomplete past
  `ImportConfirmTimeout` → `FailAttempt("import not confirmed")` + cooldown.
- **Confirm — still waiting:** `IMPORTING` job incomplete but within timeout →
  stays in `IMPORTING`, no state change.
- **Idempotency:** empty-folder shortcut in `VERIFYING` still marks `COMPLETED`.

## Affected files

- `internal/lidarr/client.go` — `AlbumStatus`, `ManualImportItem.TrackIDs`,
  `tracks[].id` parsing.
- `internal/engine/ports.go` — `MusicSource.AlbumStatus`.
- `internal/engine/discovery.go` — `advanceImporting` split into VERIFYING gate +
  IMPORTING confirm; `Advance` dispatch includes `IMPORTING`.
- `DiscovererParams` + config wiring — `ImportConfirmTimeout`.
- Corresponding tests (fake `MusicSource`/store already used in engine tests).
