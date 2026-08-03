# Postgres is the only backend; SQLite is not coming back

slusk ran on SQLite once, and `cmd/sqlite2pg` still exists to migrate a legacy file into
Postgres. Adding SQLite *back*, alongside Postgres, is a recurring request — it surfaced
again on r/Lidarr in August 2026 — and the answer is no. This records why, so the next
person to ask gets the cost inventory rather than a re-litigation.

## What was actually being asked for

Two comments. The first was mostly about the native Soulseek client's security and added
"why postgres instead of something lighter like mysql" as an aside, on a premise that does
not hold: MySQL is not lighter than Postgres in any measurable sense, and it would be a
third SQL dialect, not a smaller one. The second was the real signal, and its operative
word was `setup`: *"I dislike having to setup a separate db for my services."*

But `docker-compose.example.yml` already bundles Postgres, with a healthcheck and a
`depends_on: service_healthy` gate. Nobody following the README sets up a database. The
complaint was about a cost that the project had already removed and failed to advertise —
so the fix was the README and the example files (#404), not the persistence layer.

## What a second backend would cost

`internal/store` is not merely "Postgres-flavoured SQL". Measured, not estimated:

- **132 query call sites**, every one on `$1` placeholders. Zero `?`-style SQL exists
  outside `cmd/sqlite2pg`'s read path.
- **Constructs with no SQLite equivalent**: `jsonb` operators and
  `jsonb_array_elements(...) WITH ORDINALITY` (4 sites); bulk upserts built on
  `unnest($1::text[], $2::bigint[], ...)` (3 sites), which would need a completely
  different batching strategy; `LEFT JOIN LATERAL` (2); `DISTINCT ON` (1);
  `date_trunc(...) AT TIME ZONE` (1); and four `DO $$ ... END $$` PL/pgSQL blocks in the
  migrations.
- **13 migrations that would each need two dialects** — and a merged migration is
  immutable by this project's own rule, so every dialect difference missed is permanent.
- **16 test files** bound to a live Postgres through `internal/store/storetest`. A second
  backend doubles that matrix rather than sharing it.
- **4 sites** that read `pgconn.PgError` codes (`23505`, plus a constraint-name check) —
  Postgres semantics leaking past the store boundary into behaviour.

Locking is the one place SQLite would *not* struggle. There is no `SKIP LOCKED` anywhere;
the pipeline uses 8 blocking `FOR UPDATE` sites in a deliberate job-then-transfer order,
plus one `pg_advisory_xact_lock`. SQLite's single global write lock is strictly stronger
than each of those, so the logic would stay correct — just less concurrent. That is worth
stating plainly, because it is the strongest argument *for* SQLite and it still does not
outweigh the rest.

## Consequences

- The persistence layer stays free to use whatever Postgres offers. Query authors do not
  have to keep a second dialect in their head, and future migrations cost one file.
- Some users will pass on slusk over this. Accepted. The request came from one person, on
  a premise that a compose file already answered.
- "Add a backend on request" is explicitly not a principle here. The same thread already
  contained a MySQL request; dialect three would cost what dialect two costs.
- The `database/sql` seam is kept anyway, and not because a second driver is planned.
  `store.go` opens `sql.Open("pgx", dsn)` and uses no `pgxpool`, no `pgx.Batch`, no
  `CopyFrom`, no `pgtype`. That is worth preserving on its own terms — it keeps the store
  testable against the standard interfaces — but it should not be read as a half-built
  abstraction waiting for SQLite to arrive.
- If this is revisited, the trigger should be evidence of users blocked by Postgres, not
  a preference stated in a comment thread.
