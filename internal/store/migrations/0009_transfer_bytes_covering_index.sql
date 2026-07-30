-- The dashboard's per-job LATERAL (internal/store/dashboard.go, jobViewFrom)
-- aggregates state, bytes_done and bytes_total from transfers per candidate on
-- every GET /api/jobs, polled every 15s per open tab (#176). The existing
-- idx_transfers_candidate(candidate_id, updated_at) buys that query nothing —
-- there is no ORDER BY or range predicate on updated_at here — and none of the
-- three read columns are covered, so Postgres pays a heap fetch per matching
-- row. Add a second index keyed on candidate_id alone (no query orders or
-- filters on the other columns, so a composite key would waste B-tree
-- comparisons) with the three read columns carried in INCLUDE, letting the
-- aggregate satisfy itself from the index alone.
--
-- Deliberately does not replace idx_transfers_candidate: other queries still
-- rely on its (candidate_id, updated_at) ordering.
--
-- 0001's CREATE TABLE IF NOT EXISTS never backfills columns onto a transfers
-- table that already existed (the pre-rewrite attempt_id shape it migrates in
-- place), so bytes_done/bytes_total are not guaranteed here even though every
-- database booted since 0001 shipped has long since carried them. Add them
-- defensively, same idiom as 0001's own ADD COLUMN IF NOT EXISTS fixups, so
-- this migration does not assume a history it cannot verify.
ALTER TABLE transfers ADD COLUMN IF NOT EXISTS bytes_done  BIGINT NOT NULL DEFAULT 0;
ALTER TABLE transfers ADD COLUMN IF NOT EXISTS bytes_total BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_transfers_candidate_bytes ON transfers(candidate_id)
    INCLUDE (state, bytes_done, bytes_total);
