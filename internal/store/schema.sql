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
    UNIQUE(lidarr_album_id)
);

ALTER TABLE album_jobs ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE album_jobs ADD COLUMN artist_name TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS candidate_attempts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    album_job_id  INTEGER NOT NULL REFERENCES album_jobs(id),
    username      TEXT NOT NULL,
    score         REAL NOT NULL,
    state         TEXT NOT NULL,
    fail_reason   TEXT NOT NULL DEFAULT '',
    backoff_until DATETIME,
    created_at    DATETIME NOT NULL
);

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
