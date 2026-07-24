-- 0004: candidate metadata surfaced on the job row (issue #156).
-- Set at the SELECTING -> DOWNLOADING transition. Nullable: historical jobs
-- and jobs not yet past selection stay NULL. Presentation data, no backfill.
ALTER TABLE album_jobs
  ADD COLUMN IF NOT EXISTS year   INT,
  ADD COLUMN IF NOT EXISTS tracks INT,
  ADD COLUMN IF NOT EXISTS format TEXT;
