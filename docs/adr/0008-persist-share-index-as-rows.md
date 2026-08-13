# The share index is persisted as rows, not as the serialised browse frame

The share index has always been memory-only, so every process start rebuilds it with a
full `filepath.WalkDir` over every shared folder — millions of stat calls on an NFS-backed
library, paid again on every image bump. #497 persists it. This records why what gets
stored is one row per file rather than the finished wire frame, because the finished wire
frame is sitting right there and storing it would obviously be cheaper.

## Why the frame is the tempting answer

`scanShares` already serialises the entire `SharedFileListResponse` once per scan and
keeps it on the snapshot as `sharedFrame []byte`. Every browse request is answered by
handing that same buffer out verbatim — it is never rebuilt per request. A restore that
read one blob out of Postgres and assigned it would be a single query and no CPU at all,
against a restore from rows that has to rebuild the trigram index, the directory list and
the frame before it can answer anything.

## What we store instead

One row per indexed file — virtual path, local path, share root, size, extension and the
attributes a peer is shown — plus a single row recording the scan's time and the shared
folders it read. Everything else, the frame included, is rebuilt in memory at startup.

## Why

**A stored frame is a versioned artifact and nothing records its version.** The encoding
belongs to `soul/peer`. Any change to it — the field order, the compression envelope, the
size constants that #409 has just split into three — makes every stored frame wrong, and
the schema has no column that would say so. The obligation to bump a format version would
land on whoever next edits an encoder, which is precisely the kind of coupling that fails
quietly: the frame still deserialises, it just describes something that is no longer true.
Rows carry no encoding.

**CPU was never the complaint.** The reported problem is disk I/O against a NAS. Trading
a filesystem walk for a table read plus an in-memory rebuild spends the resource nobody
was short of.

**Rows answer more than one question.** The trigram search index, the per-folder
statistics and the directory listing are all derived from the same rows. A blob answers
exactly the one question it was serialised for, and the other three would have to be
stored separately or recomputed anyway.

## Considered and rejected

**Store the frame with a format-version column.** It works, and it is the fastest restore
available. Rejected because the version only helps if it is remembered, and the person it
has to be remembered by is whoever is editing an encoder in a different package for an
unrelated reason. The failure mode is a peer being served a stale encoding, which no test
in this repo would catch.

**Store both — rows as the source, the frame as a fast path.** Rejected as the cost of the
first option with the complexity of the second, for a saving that has not been measured to
matter.

## Consequences

- Startup becomes CPU-bound where it used to be I/O-bound. That is the whole point, but it
  is only a win if it holds: load plus rebuild must stay well under the walk it replaces,
  measured on synthetic data at the 1,000,000-file cap `maxSharedFileListFiles` imposes.
- A change to the wire format needs no migration, no invalidation and no thought about
  what is already stored.
- The table's only validity condition is the shared folder set it was scanned from. It
  deliberately does not share an invalidation rule with `share_file_metadata`, which is
  valid while a file's size and mtime match — the two caches have different lifetimes and
  merging them would give the shorter one to both.
- Bitrate and duration are therefore stored twice, once in each table. Accepted: it keeps
  the startup path a single sequential read of a single table, and duplication inside a
  cache is not a normalisation defect.
