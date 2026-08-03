# Upgrading past the pipeline state-machine rewrite

One-time, breaking upgrade note. It applies only when moving a deployment from a
release older than the `internal/pipeline` rewrite. If your `config.toml` already has a
`[pipeline]` section rather than `[engine]`, this has already been done and none of it
applies.

The rewrite replaced the old engine with `internal/pipeline` (states `WANTED`,
`SELECTING`, `DOWNLOADING`, `IMPORTING`, `DONE`, `CANCELLED`, `FAILED`) and swapped the
`candidate_attempts` table for a `candidates` table. **There is no migration path for
in-flight jobs** — the pipeline tables are clean-slated instead.

Nothing is lost by that: every still-wanted album reappears as `WANTED` on the first
`WantedSync` tick after startup, so nothing needs to be manually re-queued. Peer history
is preserved.

## Procedure

1. Stop slusk.

   ```bash
   docker compose stop slusk
   ```

2. Migrate `config.toml`:
   - `[engine]` → `[pipeline]`
   - drop `lidarr.poll_interval` and `slskd.status_poll_interval` (both gone)
   - rename `max_concurrent_active` → `max_active` (now under `[pipeline]`)
   - drop `batch`, `candidate_backoff`, `failed_candidate_backoff`, `failed_retry_after`
     and `tick_interval` (no `[pipeline]` equivalent)

   `internal/config` rejects unknown keys at startup, so a leftover key from the old
   section stops the container rather than being ignored — that error is the check that
   this step was done.

3. Wipe the pipeline tables. This clears `album_jobs`, `candidates`,
   `candidate_attempts`, `transfers` and `job_events`, and keeps `known_users` and
   `artist_user_reliability` — the accumulated peer-reliability history survives.

   ```bash
   scripts/clean-slate-pipeline.sh "$DSN"
   ```

4. Manually clear slskd's downloads directory, so leftover files from orphaned transfers
   don't collide with fresh attempts.

5. Pull and start.

   ```bash
   docker compose pull && docker compose up -d
   ```
