-- Persistent cache for the technical audio metadata scanShares extracts from
-- every shared mp3/flac (issue #197). The expensive part of a share scan is not
-- the directory walk but opening and reading each audio file, and that result is
-- a pure function of the file's contents — so it is cached here, keyed on the
-- local path and invalidated by size and mtime.
--
-- This table is a cache and nothing else: it never affects what is advertised.
-- Size, virtual paths, the trigram index and ShareStats are all still derived
-- from the filesystem on every scan, so a stale or missing row can only cost
-- time, never correctness.

CREATE TABLE IF NOT EXISTS share_file_metadata (
    -- The local filesystem path, exactly as filepath.WalkDir produced it under
    -- the resolved share root. Never placed on the wire; the virtual path is
    -- derived per scan and deliberately not stored, so renaming a share's
    -- public name costs nothing.
    path       TEXT PRIMARY KEY,
    -- size and mtime_us are the invalidation key alongside path. mtime is stored
    -- as microseconds since the Unix epoch rather than TIMESTAMPTZ on purpose:
    -- filesystem mtimes carry nanosecond precision, TIMESTAMPTZ only
    -- microseconds, and a value that did not survive the round trip byte for
    -- byte would make every lookup miss forever while still looking correct.
    -- Microseconds, not nanoseconds, so an absurd far-future mtime cannot
    -- overflow int64.
    size       BIGINT NOT NULL,
    mtime_us   BIGINT NOT NULL,
    -- bitrate in kbit/s and duration in seconds, the two peer.Attribute values
    -- that go on the wire. Both zero means "this file was examined and produced
    -- no attributes" — a cached negative result, which stops a corrupt or
    -- unparseable mp3 from being reopened on every scan. Zero is safe as that
    -- sentinel because extractTechnicalMetadata already rejects a zero bitrate
    -- or duration, so it can never be a real value.
    bitrate    BIGINT NOT NULL,
    duration   BIGINT NOT NULL,
    -- When this row was last (re)computed by reading the file. Purely
    -- diagnostic: row lifetime is governed by the exact prune set a successful
    -- scan computes, not by age.
    updated_at TIMESTAMPTZ NOT NULL
);
