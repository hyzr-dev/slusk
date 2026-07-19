# Import pipeline robustness — design

**Date:** 2026-07-19
**Status:** Approved for planning
**Touches:** `internal/lidarr/client.go`, `internal/core/models.go`, `internal/store` (schema + wanted/candidates), `internal/pipeline/wanted.go`, `internal/pipeline/discovery.go`, `internal/pipeline/importing.go`, `go.mod`

## Motivation

Soulseek is unreliable about how complete a shared folder is, and Lidarr is
strict about matching. Today's pipeline makes three simplifying assumptions
that don't hold in practice:

1. It compares candidates against **one canonical track count** per album,
   not against the band of valid release editions.
2. It has **no dedup** step, so a single Soulseek user's folder can contain
   duplicate copies of the same track (different formats, or a sloppily
   organized share) and those duplicates flow straight into the Lidarr
   import scan.
3. It trusts Lidarr's per-file `Importable` flag as-is. Lidarr stamps
   `Has unmatched tracks` on **every file in a folder** when the folder
   isn't a perfect bijection against the release — including files that
   individually matched a track correctly. A real case: 13 files, 11
   correctly matched, all 13 rejected outright by today's logic.

This design fixes all three, plus a fourth root cause found independently
during investigation. It is scoped as one design because the changes touch
adjacent, interdependent parts of the same `WANTED → SELECTING → DOWNLOADING
→ IMPORTING` flow — implementation may still land as separate PRs/commits
per section.

Explicitly **out of scope**: the manual-import flag
(`disableReleaseSwitching`) — already correct since commit `37dfbcd` (sends
`false` unconditionally, letting Lidarr pick the release dynamically). No
change needed there.

## 1. Wanted-sync: per-release min/max track count

**Problem:** `WantedAlbum.TrackCount` (client.go) and the `AlbumJob` it
seeds only ever carry one number — Lidarr's canonical album-level track
count. Different pressings/editions of the same album can legitimately
have different track counts (bonus tracks, deluxe editions, etc.), and none
of that variance is visible to the pipeline today.

**Change:**

- Add `AlbumReleases(ctx, albumID) ([]AlbumRelease, error)` to
  `internal/lidarr/client.go`, calling Lidarr's release-listing endpoint for
  an album and returning each release's track count.
- Replace `WantedAlbum.TrackCount int` with
  `WantedAlbum.MinTrackCount, MaxTrackCount int` — computed as the min/max
  across all releases returned by `AlbumReleases`.
- Add `MinTrackCount`, `MaxTrackCount` columns to `album_jobs`
  (`internal/store/schema.sql`), and matching fields on `core.AlbumJob`.
- `internal/pipeline/wanted.go`'s sync writes these two fields instead of a
  single track count, on both job creation and update.

**Fallback:** if `AlbumReleases` returns no releases (or errors), fall back
to Lidarr's existing album-level track count as both `MinTrackCount` and
`MaxTrackCount` (a degenerate single-value band) — this preserves today's
behavior when the new call isn't available.

## 2. Discovery/selecting: `[min, max]` band replaces the ratio filter

**Problem:** `discovery.go` currently fetches a single `total` via
`AlbumStatus` and accepts candidates whose file count falls in
`[total, total*ratio]` — an arbitrary multiplier used as a stand-in for
"could be a slightly different edition or contain a couple of bonus
tracks," while also being the only guard against whole-discography dumps.

**Change:** replace that band with `job.MinTrackCount <= len(cand.Files) <=
job.MaxTrackCount`, sourced from the per-release data added in section 1.
The ratio-based ceiling is removed entirely — a genuine `[min, max]` band
across real editions is expected to already exclude discography dumps
(which have far more files than any single edition), so no second filter is
layered on top.

**Fallback:** identical in spirit to today's `total == 0` skip — if
`MinTrackCount == 0 && MaxTrackCount == 0` (unknown), skip the filter
rather than reject every candidate.

## 3. Dedup: tag-based, same-download duplicate removal

**Problem:** no code anywhere in the pipeline reads embedded audio tags.
Matching is filename/size/bitrate heuristics only (`slskd.Result`,
`core.CandidateFile`). A single candidate's shared folder can contain two
copies of the same track (different formats, or a folder that's simply not
organized), and today those duplicates pass straight through into Lidarr's
import scan.

**Change:**

- New dependency: `dhowden/tag` (or equivalent), added to `go.mod`, for
  reading ID3/FLAC/MP4 tags from files already on disk.
- New file `internal/pipeline/dedup.go`, exposing
  `dedupAlbumFolder(folder string) error`:
  - Read tags from every audio file in the folder.
  - Group files into "same track" using **track number first** (disc +
    track, when both files have it set), **falling back to normalized
    title matching** (lowercase, strip special characters and `feat.`-style
    suffixes) when track numbers are missing or inconsistent — covering
    both well-tagged and messy folders.
  - Within each group, keep exactly one file: **lossless beats lossy**
    regardless of bitrate, and **bitrate is the tiebreaker within the same
    format class**. Every other file in the group is treated as a loser and
    excluded from the import scan (removed or left untouched on disk,
    implementation's choice, as long as Lidarr never sees it).
- Called as the **first step of `Importing.verify()`**
  (`internal/pipeline/importing.go`), before `ManualImportCandidates` is
  invoked — reuses `verify()`'s existing idempotent retry behavior (it
  already tolerates being re-run after a crash), so no changes are needed
  to the state machine or to `downloading.go`.

## 4. Rejection-fix: TrackID-based importable check

**Problem (found during investigation, folded into this design):**
`verify()` (importing.go:207-213) currently does:

```go
for _, it := range items {
    if it.Importable {
        importable = append(importable, it)
    } else {
        rejections = append(rejections, it.Rejections...)
    }
}
```

and then (line 214) rejects the **entire candidate** if any file produced a
rejection. But Lidarr's `Has unmatched tracks` rejection is stamped on every
file in a non-bijective folder, including files that were individually
matched to a valid track. Combined with section 3's dedup, most cases where
a folder isn't a perfect bijection will already be resolved before this
check runs — but not all (e.g. legitimate extra files that dedup has no
reason to remove).

**Change:** replace the `it.Importable` check with a `TrackIDs`-based one:

```go
for _, it := range items {
    if len(it.TrackIDs) > 0 {
        importable = append(importable, it)
    } else {
        rejections = append(rejections, it.Rejections...)
    }
}
```

A file that Lidarr assigned one or more real track IDs is treated as
importable regardless of a folder-level rejection reason attached to it.
Files with no track ID at all (genuinely unmatched) still fall through to
`rejections` and can still fail the candidate. The downstream coverage
check (`coverage(importable) < total`, importing.go:227) is unchanged and
continues to guard against importing a partial release.

## Data model summary

| Type | Field(s) added | Field(s) removed |
|---|---|---|
| `lidarr.WantedAlbum` | `MinTrackCount`, `MaxTrackCount` | `TrackCount` |
| `core.AlbumJob` | `MinTrackCount`, `MaxTrackCount` | — |
| `album_jobs` (schema.sql) | `min_track_count`, `max_track_count` | — |

No changes to `core.Candidate` / `core.CandidateFile` — dedup operates on
files already on disk via their transfer filenames, not on new persisted
fields.

## Testing

- Unit tests for `AlbumReleases` client parsing (min/max computation across
  multiple releases, including the zero-releases fallback).
- Unit tests for the `[min, max]` discovery filter, replacing the existing
  ratio-filter tests in `discovery_test.go`.
- Unit tests for `dedupAlbumFolder`: track-number grouping, title-fallback
  grouping, lossless-over-lossy priority, bitrate tiebreaker within the same
  format.
- Unit tests for the rejection-fix in `importing_test.go`: a file with
  `TrackIDs` set but a `Has unmatched tracks` rejection is kept; a file with
  no `TrackIDs` is still rejected.
- Update `internal/pipeline/integration_test.go` and
  `pipeline_shared_test.go` fixtures wherever they assume the old single
  `TrackCount` field or the old ratio filter.
