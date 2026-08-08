-- Issue #317: a candidate that already failed import for a job gets downloaded
-- again on the next search cycle.
--
-- The job's memory of what it has already tried lives entirely in `candidates`,
-- and ResetJobToWanted deletes those rows on every non-terminal retry. The
-- Soulseek query is unchanged between cycles and the network returns
-- substantially the same peers, so the next cycle re-caches - and re-downloads -
-- the very candidates that just failed. `retries` then acts as a pure countdown:
-- it delays the repetition via backoff but changes nothing about what is tried.
--
-- The fix needs state whose lifetime is the *job*, not the search cycle, which
-- is why this is its own table rather than a flag on `candidates`: those rows
-- are deliberately transient and their deletion is load-bearing elsewhere.
--
-- (album_job_id, username, release_dir) is the identity of "the same candidate":
-- username alone is too blunt (the same peer may well share the right album in a
-- different directory), and the pair is exactly the key matcher.Rank already
-- groups search results on, so one rejection matches one candidate.
--
-- ON DELETE CASCADE, unlike album_jobs' other children which DeleteJob removes
-- explicitly: retention is a requirement here, not an implementation detail. The
-- history must die with the job (a peer that fixes its share has to be reachable
-- again eventually), and cascading makes that structural rather than something a
-- future delete path can forget.
-- The natural key is the primary key: no surrogate id, because nothing
-- references a rejection row and one B-tree serves all three jobs this table
-- has - uniqueness, the ON CONFLICT target for the writer's upsert, and the
-- leading-column range scan for Discovery's one read per search cycle. The
-- upsert is not hypothetical: a job whose history was cleared by an explicit
-- retry can fail the same pair again.
CREATE TABLE IF NOT EXISTS candidate_rejections (
    album_job_id BIGINT      NOT NULL REFERENCES album_jobs(id) ON DELETE CASCADE,
    username     TEXT        NOT NULL,
    release_dir  TEXT        NOT NULL,
    reason       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (album_job_id, username, release_dir)
);
