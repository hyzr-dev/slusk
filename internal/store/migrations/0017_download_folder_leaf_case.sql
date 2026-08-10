-- job_download_folders was unique on (album_job_id, leaf), a plain
-- case-sensitive comparison. On a case-insensitive download root two casings of
-- one directory therefore registered as two rows: cleanupFolder stamped
-- cleaned_at on whichever it processed, and the other stayed uncleaned forever,
-- pointing at a directory that was already gone (issue #479).
--
-- That matters beyond tidiness. An uncleaned row is what the ownership rule in
-- #471 reads to decide whether a job still holds a folder, so a permanently
-- uncleaned duplicate would become a permanent phantom owner.
--
-- The stored value keeps its original casing throughout. It is handed verbatim
-- to DeleteDownloadFolder, which base64-encodes it for slskd, so a lowercased
-- value would 404 against a case-sensitive filesystem. The column is an address
-- at the backend, not an identity slusk owns: lower() belongs in the index and
-- in comparisons, never in the data.
--
-- The old (album_job_id, leaf) constraint is deliberately LEFT IN PLACE. It is
-- implied by the new index and therefore redundant, but during a rolling deploy
-- the older instance is still running `ON CONFLICT ON CONSTRAINT
-- job_download_folders_job_leaf_key`, and dropping the constraint out from under
-- it would break every registration it attempts.

-- Step 1: collapse each group of case-variant rows onto its survivor, BEFORE
-- deleting anything — once the duplicates are gone the group's state cannot be
-- recomputed.
--
-- The survivor is the OLDEST row in the group, which is what runtime semantics
-- already imply: the row is created by a job's first registration of a folder,
-- and a later registration of the same folder does not rewrite its name. Picking
-- the newest instead would leave the migration and RegisterDownloadFolder
-- disagreeing about which casing wins.
--
-- What the survivor does take from the group is its most cautious cleaned_at:
-- NULL if ANY row in the group is uncleaned. Taking the oldest row's
-- own cleaned_at instead would let a cleaned duplicate hide a folder that is
-- still on disk, which is precisely the leak #314 exists to close — a folder no
-- row can name is a folder nothing will ever delete.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY album_job_id, lower(leaf)
                              ORDER BY created_at, id) AS rn,
           count(*)      OVER (PARTITION BY album_job_id, lower(leaf)) AS group_size,
           bool_or(cleaned_at IS NULL)
                         OVER (PARTITION BY album_job_id, lower(leaf)) AS any_uncleaned,
           max(cleaned_at)
                         OVER (PARTITION BY album_job_id, lower(leaf)) AS newest_cleaned
    FROM job_download_folders
)
UPDATE job_download_folders f
SET cleaned_at = CASE WHEN r.any_uncleaned THEN NULL ELSE r.newest_cleaned END
FROM ranked r
WHERE f.id = r.id
  AND r.rn = 1
  AND r.group_size > 1;

-- Step 2: drop the non-survivors. The window definition repeats step 1's
-- deliberately: the two statements must partition identically, and a shared CTE
-- is not available across statements.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY album_job_id, lower(leaf)
                              ORDER BY created_at, id) AS rn
    FROM job_download_folders
)
DELETE FROM job_download_folders f
USING ranked r
WHERE f.id = r.id
  AND r.rn > 1;

-- Step 3: make it an invariant of the table rather than something every consumer
-- must re-check, the same argument 0015's CHECK was added under. Not
-- CONCURRENTLY: the migration runner applies each file inside one transaction,
-- and this table is small.
CREATE UNIQUE INDEX IF NOT EXISTS job_download_folders_job_lower_leaf_key
    ON job_download_folders (album_job_id, lower(leaf));
