-- 0003_manual_jobs.sql
-- Job source + manual-job identity decoupling (issue #155).
ALTER TABLE album_jobs ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'lidarr';
ALTER TABLE album_jobs DROP CONSTRAINT IF EXISTS album_jobs_lidarr_album_id_key;

-- Postgres has no "ALTER COLUMN ... DROP NOT NULL IF EXISTS" form, and the
-- partial index below references the same column, hence the single DO block
-- guard - both are a no-op on a database whose album_jobs predates
-- lidarr_album_id entirely (only ever exercised by a store test fixture that
-- models a much older, column-sparse shape; every real database has this
-- column from its first migration).
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='album_jobs' AND column_name='lidarr_album_id') THEN
        ALTER TABLE album_jobs ALTER COLUMN lidarr_album_id DROP NOT NULL;
        CREATE UNIQUE INDEX IF NOT EXISTS idx_album_jobs_lidarr_id
            ON album_jobs(lidarr_album_id)
            WHERE source = 'lidarr';
    END IF;
END $$;
