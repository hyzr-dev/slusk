# `queued` was redefined to its literal meaning, and `waiting` was split out beside it

`dashboardJobStatusSQL` (`internal/store/dashboard.go:144`) mapped three different
situations onto one status, `queued`: a job that had never been searched (`WANTED`), a
job with candidates cached but waiting for a `max_active` slot (`SELECTING`), and a job
mid-download with no file currently in progress. "Why is nothing happening?" has two
different answers hiding in there — no candidates yet, versus a config knob (`max_active`)
being full — and the dashboard gave the same word for both.

Worse, the word itself was backwards. In Soulseek, "queued" means standing in a peer's
queue for a file — which is exactly the case the old label did *not* single out; it was
buried inside the same bucket as everything else. Splitting `WANTED` and `SELECTING` out
as their own statuses answers the "why is nothing happening" question honestly. The
remaining `DOWNLOADING` gap splits in two on whether the peer has delivered anything yet:
`queued` names a job whose candidate is chosen but whose files are all still sitting in
that peer's queue, none arrived, and `waiting` names a job that has already received at
least one file from the candidate and has nothing moving right now — the gap between two
files of the same candidate. `queued` gets the word Soulseek users already associate
with peer-queue standing; `waiting` gets the plainer word for a pause with no clean second
meaning to steal.

## The first naming attempt was refuted by measurement

The initial version of this decision named the two split-out states `preparing` (candidate
chosen, nothing delivered yet) and `queued` (at least one file delivered, none moving). The
name `preparing` rested on an assumption, stated plainly in the first draft's consequences
section, that the pre-first-file window was a startup blip — milliseconds, not worth a
prominent word.

A six-minute lab run against real Soulseek peers (60 samples, one every 6 seconds)
measured the opposite. The state that was to become `queued` — a candidate chosen, no
file yet arrived — was present in 60 of 60 samples: the same job, one peer, six minutes
without a single file arriving. `selecting`, assumed to be the long-lived one waiting on
a `max_active` slot, appeared in only 8 of 60. `active` appeared in 42/60 and the state
that was to become `waiting` in 38/60. The prediction the names were built on was wrong in
the direction that mattered most: the state calling itself "preparing" was the longest-
lived of the four, not the shortest, and "preparing" additionally implied slusk was doing
something during that window, when in fact the peer holds the initiative and slusk is
idle. The names were changed to `queued`/`waiting` before merge on this evidence, not
polished afterward — this is a correction, not a refinement.

## Considered and rejected

**A new job state between `SELECTING` and `DOWNLOADING`.** The "between files from the
same candidate" case that needed splitting out happens *inside* `DOWNLOADING` — a job
with a candidate chosen keeps re-entering that gap every time one file finishes and the
next starts. A job state cannot express that without flapping between two states on every
top-up, and `max_active` counts `DOWNLOADING` + `IMPORTING` rows, so a flapping state
would miscount the very cap this vocabulary exists to make legible. This is presentation
work for exactly that reason: no new `core.AlbumJobState`, no migration.

**Keep `queued` as an umbrella filter value covering all four, label narrowly in the
UI.** Rejected because it recreates the second-source-of-truth problem #269 already
deleted: the `queued` facet count and the rows a `filter=queued` request actually returns
would disagree the moment the label and the filter stop being the same predicate.

**Leave `queued` alone and invent a new word for the peer-queue case.** Rejected. It
keeps the misleading label permanently — three unrelated situations still read
identically — and the one honest word for "our request is in a peer's queue" was already
taken by the wrong case. A new word would have solved the ambiguity but not the mislabel.

## Consequences

- `GET /api/jobs?filter=queued` now returns a different, much smaller population — the
  narrower pre-first-file case — and the `queued` facet in `DashboardStatusFacets` counts
  the same narrower set. No error, no compatibility layer — a different, correct answer.
  The embedded UI ships in the same binary as the backend, so it can never itself fall out
  of step with this change; the actual exposure is external scripts hitting the API
  directly, and the promotion release notes are the only channel that reaches them.
- The frontend's client-side `queuePosition` override — `tagFor`'s special case in
  `web/src/components/tui/Tag.tsx` and `rowTone`'s `inPeerQueue` in
  `web/src/routes/Jobs.tsx` — is deleted. It was a fourth derivation of "queued" that
  could disagree with the backend, the same Go/TS double-track #269 removed elsewhere.
  The backend is now the only place the concept is derived.
- The four statuses' relative dwell times are no longer a guess derived from reading
  `internal/pipeline/downloading.go` — they were measured against real Soulseek peers in
  the `testenv` lab, and `queued` (formerly `preparing`) turned out to be the longest-lived
  of the four rather than the shortest. That single six-minute, 60-sample run shows the
  shape of the cycle, not a stable distribution — peer availability and transfer speed vary
  run to run — so it should be read as evidence that the earlier assumption was wrong, not
  as a number to design further UI around without remeasuring.
