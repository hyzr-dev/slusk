# Migrating the store from SQLite to PostgreSQL

As of the Postgres port, slusk persists all state in PostgreSQL
(`store.dsn` in `config.toml`) instead of a local SQLite file. Existing
SQLite databases are migrated once with the `sqlite2pg` tool.

## Supported SQLite database

The tool supports the final SQLite schema released immediately before the
PostgreSQL port. It must contain `album_jobs`, `candidate_attempts`, `transfers`,
`known_users`, `artist_user_reliability`, and `job_events` with that release's
complete column set. The source is checked before the PostgreSQL target is
opened; older, newer, incomplete, orphaned, or unknown-state sources fail with
an actionable `unsupported SQLite schema` error rather than being partially
copied. Upgrade an older database by running the final SQLite release against a
backup first, then migrate the upgraded file.

The schema rewrite is transformed as follows:

- every `candidate_attempts` row becomes a `candidates` row with the same ID,
  album-job relationship, username, score, terminal state, failure reason, and
  timestamps; its cached `files` JSON is reconstructed losslessly from that
  attempt's transfers (`filename` and `bytes_total`). The current model has no
  per-candidate scheduling field (its backoff is job-scoped), so a source with
  any non-NULL `candidate_attempts.backoff_until` is rejected rather than
  silently dropping the timestamp or changing its scope;
- legacy `PENDING` attempts become `ACTIVE`: `CreateAttempt` wrote `PENDING` and
  the legacy engine never promoted that column, so it denoted the selected,
  active attempt. Existing `ACTIVE`, `SUCCEEDED`, and `FAILED` meanings are
  retained;
- `transfers.attempt_id` becomes `transfers.candidate_id`, retaining transfer
  IDs and all other transfer fields;
- job states map to the current owners: `DISCOVERED`/`SEARCHING`/legacy
  `SELECTING` to `WANTED`, `VERIFYING` to `IMPORTING`, `COMPLETED` to `DONE`,
  and `COOLDOWN` to `WANTED` with `next_attempt_at` also stored as
  `not_before`. `DOWNLOADING`, `IMPORTING`, `FAILED`, and `CANCELLED` retain
  their meaning; `FAILED.updated_at` becomes `failed_at`. A legacy `IMPORTING`
  job's active candidate receives `import_submitted_at` from the job transition
  timestamp so Lidarr is not submitted twice.

## Steps

1. Stop slusk (the SQLite file must not be written during the copy).
2. Create an empty database and user on your Postgres instance, e.g.:

   ```sql
   CREATE USER slusk WITH PASSWORD 'password';
   CREATE DATABASE slusk OWNER slusk;
   ```

3. Build and run the migration tool once:

   ```bash
   go build -o sqlite2pg ./cmd/sqlite2pg
   ./sqlite2pg \
     -sqlite /path/to/slusk.db \
     -pg 'postgres://slusk:password@localhost:5432/slusk?sslmode=disable'
   ```

   The tool validates the source, applies the current schema, refuses to run
   against a target that already has rows, transforms and copies every table in
   one transaction preserving primary keys and foreign keys, validates the
   transfer-to-candidate constraint, resets the identity sequences, and logs
   per-table row counts.

4. Update `config.toml`: replace the old `[store] path = "..."` key with

   ```toml
   [store]
   dsn = "postgres://slusk:password@postgres:5432/slusk?sslmode=disable"
   ```

5. Start slusk and verify with `psql ... -c 'SELECT count(*) FROM album_jobs'`
   and the dashboard. The old SQLite file can then be archived or deleted.
