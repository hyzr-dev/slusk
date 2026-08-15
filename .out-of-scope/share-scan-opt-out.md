# Opting out of the startup share scan

slusk has no configuration key for "do not scan shares at startup", and is not going to
grow one. The startup cost that such a key would avoid is already gone, and what the key
would still buy is not a cheaper startup — it is *sharing nothing at all*, which is what
leaving `soulseek.shared_folders` empty already expresses.

## Why this is out of scope

The request originally made sense. Before #497 the share index died with the process, so
every restart walked the whole library again — painful on a large collection on a NAS.
slskd has `--no-share-scan` for exactly that, and copying the lever was the obvious move.

#497 removed the reason instead of adding the lever. The index now lives in Postgres and
`runInitialShareScan` publishes it without touching the filesystem
(`internal/soulseek/shares.go`, `internal/soulseek/shareindex.go`):

```go
// runInitialShareScan, internal/soulseek/shares.go
loaded, err := c.loadAndPublishShareIndex(ctx)
if err != nil {
    // permanent (ErrShareTooLarge) — a scan would fail identically
    return
}
if loaded {
    // the filesystem was never read
    return
}
// ... only now does the full walk begin
```

A full walk survives in exactly four cases, each logged with its reason by
`loadShareIndex`:

1. no index has been stored yet,
2. `shared_folders` changed since the index was written,
3. the stored index is incomplete (`FileCount != len(Files)`),
4. reading the index out of Postgres failed.

In all four, walking the filesystem is the correct thing to do — there is no trustworthy
index to reuse. A `scan_shares_on_startup = false` would mean "skip the walk in those
cases too", i.e. come up with an empty share and stay that way until somebody opens the
dashboard and clicks rescan. For a headless deployment on an auto-updater that silently
means sharing is off permanently, which nobody asked for.

## The cost is not just the key

`internal/config` rejects unknown keys and merging deploys, so a new key is never only a
new key:

- `/api/shares` would need a new field. Zero folders already has four distinct causes,
  and `web/src/routes/Shares.tsx` disambiguates them in one chained ternary
  (`lastError` → `scanning` → empty config → `stale`). With the flag on and no persisted
  index the user falls into the empty-config branch and is told *"No shared folders
  configured"* plus a config snippet — for folders they have already configured. That is
  the #408 failure mode again, so a fifth branch and a fifth backend signal would be
  mandatory, not polish.
- A `WARN` at startup, so headless users learn sharing is off.

That is three surfaces and a new piece of user-visible copy, to make optional a scan that
in the steady state no longer runs.

## If this comes back

The thing to ask for is not the flag. It is whichever of the four fallback cases actually
hurt — most plausibly case 2, where changing one share path re-walks everything instead of
reconciling the diff. That is a real issue about incremental indexing, and it is worth
opening on its own terms. "Let me turn the scan off" is a way of describing that pain, not
a fix for it.

## Prior requests

- #498 — "feat: make the startup share scan optional (scan_shares_on_startup)"
