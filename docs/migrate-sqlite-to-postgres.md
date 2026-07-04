# Migrating the store from SQLite to PostgreSQL

As of the Postgres port, slskdarr persists all state in PostgreSQL
(`store.dsn` in `config.toml`) instead of a local SQLite file. Existing
SQLite databases are migrated once with the `sqlite2pg` tool.

## Steps

1. Stop slskdarr (the SQLite file must not be written during the copy).
2. Create an empty database and user on your Postgres instance, e.g.:

   ```sql
   CREATE USER slskdarr WITH PASSWORD 'password';
   CREATE DATABASE slskdarr OWNER slskdarr;
   ```

3. Build and run the migration tool once:

   ```bash
   go build -o sqlite2pg ./cmd/sqlite2pg
   ./sqlite2pg \
     -sqlite /path/to/slskdarr.db \
     -pg 'postgres://slskdarr:password@localhost:5432/slskdarr?sslmode=disable'
   ```

   The tool applies the current schema, refuses to run against a target that
   already has rows, copies every table in one transaction preserving primary
   keys, resets the identity sequences, and logs per-table row counts.

4. Update `config.toml`: replace the old `[store] path = "..."` key with

   ```toml
   [store]
   dsn = "postgres://slskdarr:password@postgres:5432/slskdarr?sslmode=disable"
   ```

5. Start slskdarr and verify with `psql ... -c 'SELECT count(*) FROM album_jobs'`
   and the dashboard. The old SQLite file can then be archived or deleted.
