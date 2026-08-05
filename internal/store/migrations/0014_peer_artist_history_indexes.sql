-- Issue #424 moves a peer's per-artist reliability history off the /api/peers
-- list and onto its own endpoint, fetched only when a row is expanded. That
-- turns one unscoped scan of artist_user_reliability per list request into a
-- lookup of a single peer's rows, and adds an artist-name lookup that did not
-- exist before. Neither is served by an existing index.

-- artist_user_reliability's only useful index is UNIQUE(artist_id, user_id).
-- A B-tree cannot skip its leading column, so "every row for this user" is a
-- sequential scan of the whole table today. Key on user_id alone (nothing
-- filters or orders on a second column here) and carry the read columns in
-- INCLUDE so an expanded peer row is answered from the index alone.
CREATE INDEX IF NOT EXISTS idx_artist_user_reliability_user
    ON artist_user_reliability(user_id)
    INCLUDE (artist_id, success_count, fail_count, last_success_at, last_fail_at);

-- The artist's display name is denormalized onto album_jobs.artist_name; there
-- is no artists table. album_jobs is indexed on state only, so resolving names
-- would otherwise cost one sequential scan of album_jobs per artist row in an
-- expanded peer.
--
-- Partial on artist_name <> '' because that is exactly the predicate the
-- lookup uses: the column's DEFAULT '' means "no name known", not a nameless
-- artist, and rows carrying it must never be picked as an answer. Keeping them
-- out of the index also keeps it small — most album_jobs rows share the
-- handful of artists a library actually tracks.
CREATE INDEX IF NOT EXISTS idx_album_jobs_artist_name
    ON album_jobs(artist_id, artist_name)
    WHERE artist_name <> '';
