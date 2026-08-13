-- The persisted share index (issue #497). Every process start used to rebuild
-- the index with a full filepath.WalkDir over every shared folder — millions of
-- stat calls against an NFS-backed library, paid again on every image bump.
-- These tables let a restart read the index back instead of reading the disk.
--
-- This is NOT a cache in front of the filesystem the way share_file_metadata
-- (migration 0007) is. share_file_metadata only ever saves time: a missing row
-- costs a file re-read and nothing else. These tables are what peers are
-- actually served after a restart, so a row that is wrong is a wrong answer on
-- the wire. The two therefore do not share an invalidation rule and deliberately
-- do not share a table: share_file_metadata is valid while a file's size and
-- mtime match, this index is valid while the shared folder set matches.
--
-- Invariant: these tables are the result of the latest complete share scan.
-- There is no incremental diff and no partial write — a save deletes everything
-- and reinserts, in one transaction. Anything else would leave a half-index
-- that looks complete.
--
-- See docs/adr/0008-persist-share-index-as-rows.md for why the serialised
-- browse frame is rebuilt from these rows rather than stored as a blob.

-- One row per indexed file, in the shape the in-memory index needs, so the
-- startup path is a single sequential read of one table rather than a join
-- across tables with different lifetimes.
CREATE TABLE IF NOT EXISTS share_index_files (
    -- The public path this file is offered under, e.g. `Music\Artist\Album\01.flac`.
    -- Backslash-separated because that is the Soulseek wire format, and it is
    -- the in-memory index's own key, so it is the primary key here too.
    virtual_path TEXT PRIMARY KEY,
    -- The local filesystem path the bytes are read from when a peer downloads
    -- this file. Never placed on the wire.
    local_path   TEXT NOT NULL,
    -- The symlink-resolved absolute share root local_path must stay beneath.
    -- Stored rather than re-derived because the upload path checks every opened
    -- file against it, and re-resolving a root at load would be exactly the
    -- filesystem work this table exists to avoid.
    share_root   TEXT NOT NULL,
    -- The advertised size in bytes, and the file's mtime as microseconds since
    -- the Unix epoch. Together they are what the upload path re-checks before
    -- serving bytes, so a file that changed since the scan is refused instead of
    -- streamed at the wrong length. Microseconds for the same reason as
    -- share_file_metadata.mtime_us: TIMESTAMPTZ would silently truncate
    -- nanosecond filesystem mtimes and make every comparison fail forever.
    size         BIGINT NOT NULL,
    mtime_us     BIGINT NOT NULL,
    -- The lowercased extension without its dot, as it goes on the wire.
    extension    TEXT NOT NULL,
    -- bitrate in kbit/s and duration in seconds, the two peer.Attribute values
    -- a peer is shown. Deliberately duplicated from share_file_metadata: the two
    -- tables have different validity conditions, and denormalising them here is
    -- what keeps the startup path one sequential read. Both zero means the file
    -- carries no attributes, exactly as in share_file_metadata.
    bitrate      BIGINT NOT NULL,
    duration     BIGINT NOT NULL
);

-- Every directory the share scan walked, including the ones holding no files.
-- They cannot be derived from share_index_files: an artist folder containing
-- only album subfolders has no files of its own, and dropping it would both
-- shrink the directory count announced to the server and remove the folder from
-- what peers see when they browse.
CREATE TABLE IF NOT EXISTS share_index_directories (
    virtual_path TEXT PRIMARY KEY
);

-- The single row describing the scan that produced the rows above. Its presence
-- is what makes the index loadable at all, and its shared_folders value is the
-- index's only validity condition.
CREATE TABLE IF NOT EXISTS share_index_scan (
    -- Single-row table: the CHECK plus the primary key make a second row
    -- impossible, so "the latest scan" cannot become ambiguous.
    id               BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    -- When the share scan finished, and how long its filesystem walk took.
    -- Both are reported unchanged after a load: the dashboard must never claim
    -- the library was read at boot when it was not.
    scanned_at       TIMESTAMPTZ NOT NULL,
    scan_duration_ms BIGINT NOT NULL,
    -- The shared folders this scan read, as a JSON array of {name, path}.
    -- Stored in full rather than hashed on purpose: when a loaded index is
    -- rejected, the log has to be able to say *what* differed, and a hash can
    -- only ever say "different".
    shared_folders   JSONB NOT NULL,
    -- The number of file rows and their total advertised size. file_count is
    -- checked against the rows actually read at load: a mismatch means the
    -- index is not the complete result of one scan, and it is discarded in
    -- favour of a full scan.
    file_count       BIGINT NOT NULL,
    total_bytes      BIGINT NOT NULL
);
