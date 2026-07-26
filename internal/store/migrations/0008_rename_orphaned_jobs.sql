-- Rename the deprecated holding-state spelling without touching lifecycle
-- timestamps. New code writes PARKED; readers continue to accept ORPHANED for
-- databases or external writes that predate this migration.
UPDATE album_jobs SET state = 'PARKED' WHERE state = 'ORPHANED';
