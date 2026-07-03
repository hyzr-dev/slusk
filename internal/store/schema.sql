CREATE TABLE IF NOT EXISTS album_jobs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    lidarr_album_id  INTEGER NOT NULL,
    state            TEXT NOT NULL,
    candidates_tried INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  DATETIME,
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    artist_name      TEXT NOT NULL DEFAULT '',
    release_date     TEXT NOT NULL DEFAULT '',
    artist_id        INTEGER NOT NULL DEFAULT 0,
    UNIQUE(lidarr_album_id)
);

ALTER TABLE album_jobs ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE album_jobs ADD COLUMN artist_name TEXT NOT NULL DEFAULT '';
ALTER TABLE album_jobs ADD COLUMN release_date TEXT NOT NULL DEFAULT '';
ALTER TABLE album_jobs ADD COLUMN artist_id INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS candidate_attempts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    album_job_id  INTEGER NOT NULL REFERENCES album_jobs(id),
    username      TEXT NOT NULL,
    score         REAL NOT NULL,
    state         TEXT NOT NULL,
    fail_reason   TEXT NOT NULL DEFAULT '',
    backoff_until DATETIME,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);

ALTER TABLE candidate_attempts ADD COLUMN updated_at DATETIME;
UPDATE candidate_attempts SET updated_at = created_at WHERE updated_at IS NULL;

CREATE TABLE IF NOT EXISTS transfers (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id       INTEGER NOT NULL REFERENCES candidate_attempts(id),
    slskd_id         TEXT NOT NULL DEFAULT '',
    username         TEXT NOT NULL,
    filename         TEXT NOT NULL,
    state            TEXT NOT NULL,
    bytes_done       INTEGER NOT NULL DEFAULT 0,
    bytes_total      INTEGER NOT NULL DEFAULT 0,
    deadline         DATETIME NOT NULL,
    last_progress_at DATETIME,
    updated_at       DATETIME NOT NULL,
    retries          INTEGER NOT NULL DEFAULT 0,
    UNIQUE(username, filename)
);

ALTER TABLE transfers ADD COLUMN retries INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_transfers_slskd_id ON transfers(slskd_id);
CREATE INDEX IF NOT EXISTS idx_jobs_state ON album_jobs(state);

-- Covering indexes for the per-job "latest attempt" / "latest transfer"
-- correlated subqueries (store/dashboard.go jobViewSelect) and the engine's
-- AttemptsForJob/TransfersForAttempt lookups. Without these, each subquery is
-- a full table scan executed once per album_jobs row, which at a few thousand
-- jobs turned one dashboard /api/jobs request into tens of seconds of CPU.
CREATE INDEX IF NOT EXISTS idx_attempts_job ON candidate_attempts(album_job_id, created_at);
CREATE INDEX IF NOT EXISTS idx_transfers_attempt ON transfers(attempt_id, updated_at);

-- known_users and artist_user_reliability hold the running success/fail history
-- of Soulseek peers, used to score-boost known-good peers (and suppress
-- consistently-failing ones) in future candidate ranking. CRITICAL: these two
-- tables are the ONLY peer history that survives a failed-album retry.
-- ResetJobForRetry DELETEs a job's candidate_attempts (and transfers) in a
-- transaction, so peer history must NOT be derived/recomputed from attempts -
-- it is written incrementally at attempt completion (RecordAttemptOutcome) and
-- kept here across retry cycles. Decay is computed in Go from the *_at
-- timestamps at ranking time, so nothing here is a pre-aged score.

-- known_users is the global reliability per username (unique across all artists).
CREATE TABLE IF NOT EXISTS known_users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT NOT NULL UNIQUE,
    success_count   INTEGER NOT NULL DEFAULT 0,
    fail_count      INTEGER NOT NULL DEFAULT 0,
    last_success_at DATETIME,
    last_fail_at    DATETIME,
    updated_at      DATETIME NOT NULL
);

-- artist_user_reliability is a junction table: one row per (artist, user) pair,
-- holding that peer's history for that specific artist (the strong signal that
-- takes precedence over the global known_users row).
CREATE TABLE IF NOT EXISTS artist_user_reliability (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    artist_id       INTEGER NOT NULL,
    user_id         INTEGER NOT NULL REFERENCES known_users(id),
    success_count   INTEGER NOT NULL DEFAULT 0,
    fail_count      INTEGER NOT NULL DEFAULT 0,
    last_success_at DATETIME,
    last_fail_at    DATETIME,
    updated_at      DATETIME NOT NULL,
    UNIQUE(artist_id, user_id)
);

-- job_events is an append-only audit trail of key pipeline decisions (search,
-- candidate selection, attempt outcomes, imports), surfaced by the dashboard's
-- Händelser (event timeline) tab and per-job detail panel. Pruned by the
-- engine loop on a fixed retention window (see Store.PruneJobEvents).
CREATE TABLE IF NOT EXISTS job_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    album_job_id  INTEGER NOT NULL,
    event         TEXT NOT NULL,
    detail        TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_job_events_job ON job_events(album_job_id, created_at);
CREATE INDEX IF NOT EXISTS idx_job_events_created ON job_events(created_at);
