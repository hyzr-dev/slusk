# A job told the same thing N times is parked, not re-searched

Job 44906 ran fifty-nine identical cycles between 2026-07-11 and 2026-08-09: select
candidate, download eighteen files, reject at import for covering 7/18 tracks,
re-search, select the same shape of candidate again. Four weeks of downloads for an
answer that never changed.

The existing retry budget cannot catch this, and that is by design rather than by
oversight. `InsertCandidates` sets `retries = 0` on every successful search because
`retries` counts *search* failures, and a search that returns candidates succeeded. A
job that finds candidates, downloads one and has it rejected at import therefore never
accumulates a single retry. `max_retries` is the wrong bound for this loop shape, so
this is a second bound with its own counter, and `retries` keeps meaning what it says.

The rule: when a job has had N candidates rejected carrying the same reason, park it
instead of re-searching.

## It lands in `PARKED`, not `IMPORT_REFUSED` — read this before #470

Issue #470 states in writing that this work lands in `IMPORT_REFUSED` and that #472 is
blocked on it. Both statements are obsolete, and anyone reading the two issues in order
will hit them. The block is gone; the two are siblings.

`IMPORT_REFUSED` is actively wrong for a capped job. Its definition asserts *the download
was complete and correct*, which for a job capped by the coverage gate is false, and its
retry route moves the job back to `IMPORTING` with the same files — retrying something
already proven insufficient. #470's own case is the opposite: there the candidate is fine
and Lidarr says no, so that reasoning is specific to #470 and does not transfer.

`CONTEXT.md` already defined **Parked** as *a job set aside because no candidate could
satisfy it, distinct from failed — parked jobs await a person's decision, failed jobs
await a retry*. That is this outcome verbatim, and it needs no new plumbing:
`RetryFailedJob` accepts `PARKED`, `SyncWantedJobs`' revival touches only `FAILED` so
there is no automatic loop, `WantedSync` cancels it when the album stops being wanted,
and the frontend already has its chip, facet, filter allowlist and retry route.
`state.go`'s doc comment had described `PARKED` far more narrowly than the glossary —
it documented its only producer at the time — and was widened to match.

## The counter already existed, with exactly the right lifetime

`candidate_rejections` (#317, migration 0016) is written only by
`RejectCandidateAndAdvance`, survives `ResetJobToWanted`, and is cleared by
`RetryFailedJob` and both `SyncWantedJobs` re-entry branches. (Not by *every* path that
resets `retries`: `RetryManualJob` resets retries and keeps the rejections. Nothing
depends on that today — a manual job has one candidate and `Selecting` fails it on the
first rejection, so it cannot reach a cap — but the shorter claim is false.) So
`SELECT count(*) … WHERE album_job_id = $1 AND reason = $2` is
the whole mechanism: no migration, no new column, no counter to maintain, and a fresh
search cycle structurally cannot reset the count.

Two consequences of reusing it, both accepted:

- It counts *distinct candidates rejected for this reason during this attempt*, not
  *consecutive* rejections. Streaks do not break — a job alternating between two dead
  ends is no less stuck.
- The reason key is `core.RejectionReason`, whose values are today's exact literals. It
  is deliberately not a normalised `job_events.detail`: that string is written for
  humans and embeds a folder path and per-candidate numbers, so re-wording it would
  silently disable the cap.

## N = `MaxCandidatesPerAlbum + 1`, derived rather than configured

Six under the default config. The number encodes the intent — one full search cycle was
exhausted and the next one said the same thing.

N = 3, the original proposal, is wrong on this substrate. `max_candidates_per_album`
defaults to 5, so three strikes would park a job partway through its *first* cycle,
having never re-searched. Partial shares are ordinary on Soulseek and the coverage gate
rejects every one, so an album whose fourth candidate is complete — a case that succeeds
today — would be parked before the fourth was tried.

No new config key. Merging deploys here, and a required key nobody will tune is cost with
no return.

## Only content faults are counted

Scope is the two call sites of `failCandidate(..., contentFault: true)` — `import
rejected` and `incomplete download` — so it is enforced structurally rather than by a
list that must be kept in sync.

`transfer failed` and the five `escalateIfStuck` reasons are excluded because they fire
when Lidarr or the network does not answer, and are therefore **correlated across every
job at once**. Capping on them would march the whole library into a state with no
automatic revival the first time Lidarr is down or a disk fills. `failCandidate`'s own
doc comment already drew this line for blacklisting and lands on the same side.
`import not confirmed` is held in reserve; it reaches the same store method by a
different route and can be added if the lab shows it is needed.

`MaxCandidates` unset means no cap at all, rather than N = 1. A caller that forgets to
wire it loses a safety net; the other reading would silently drive every job into
`PARKED`.

## Retry and re-run behave differently on a parked job

This is invisible in the interface, so the copy now says it. **Retry** clears the
rejection history: the job gets a full N again and may re-download peers that already
failed. **Re-run pipeline** — the control #376 renamed from "Force search", and the name
the copy must use, because copy pointing at a button that is not on screen misdirects —
deliberately keeps the history (#317). It searches fresh and tries only peers not yet
tried, but since the count is already at N it re-parks on the first rejection. It is a
one-shot probe. Both are kept: they answer different user beliefs, and re-running the
pipeline is the only manual lever for jobs in `COOLDOWN`/`SELECTING`/`DONE`, which retry
does not accept.

Neither sentence is true for a **manual** job, which reaches `PARKED` only through the
transfer path: `JobActions` does not offer it "Re-run pipeline" at all, and its Retry is
`RetryManualJob`, which keeps the single candidate and re-downloads from the same peer
instead of starting a fresh search. `ParkedExplanation` therefore takes the job's source
and renders one of two strings.

`web/src/strings.ts`'s `parkedExplanation` previously hardcoded a *cause* — "repeated
backend disappearance exhausted transfer retries" — true while the lost-transfer path was
the only producer, and a fabricated diagnosis for every capped job afterwards. Note that
"no candidate could satisfy this album" is equally a cause, and equally wrong for the two
transfer paths, which never inspect a candidate's content: with three producers the copy
has to name none of them and point at the job's own events, which do know. The guard test
is an allowlist of banned causal grammar rather than a blocklist of the phrases previous
versions happened to get wrong.

## The cap is retroactive on deploy, deliberately

Reusing today's reason strings means the cap counts rows already in the database, so
every job already holding N matching rows parks on the first tick after deploy. Expect a
visible batch on the canary rather than a trickle. Those are precisely the jobs this
exists to stop, and parking is one click from reversible.

## Jobs parking here are a signal, not a new problem

The underlying defect behind job 44906 is #280. This is the safety net that would have
capped its cost at six downloads instead of fifty-nine, without anyone knowing what #280
was. A rising count of jobs parked for `incomplete download` means #280 is still live.

## The count that decides and the count that is reported are read twice

The cap acts on `prior + 1` — the history plus the candidate being rejected right now,
which is not yet committed. That estimate is right for the *decision*: it errs toward
parking a job that has heard the same answer enough times. It is not safe to *report*,
because the store records nothing for a candidate whose cached files carry no parseable
filename and its upsert adds no row for a (peer, folder) pair already present. So the
`job_parked` detail re-reads the count after the transaction commits, and omits the
number entirely if that read fails. A trail that says "6 rejected" over a history holding
five is a fabricated number in the one place a troubleshooter reads as fact.

## The event is not yet on the jobs list

`job_parked` was added to `failureExplainingEvents`, but that lookup only runs for rows
whose dashboard status is `failed`, and `PARKED` maps to `parked`. The entry is correct
classification and pays off for a job that parks and later fails; it does not by itself
put the park reason on any screen — nothing renders `failDetail` for a parked job
either. Kept rather than reverted, and surfacing it is tracked as #484.
