# A new terminal state, `IMPORT_REFUSED`, marks a permanent Lidarr refusal

No existing job state meant "the download was complete and correct, and Lidarr
permanently refused to accept it." Before this decision there were four terminal
states — `DONE`, `FAILED`, `CANCELLED`, `NOT_IMPORTED` — and a fifth is not a change to
make lightly, so it needs its own reasons on record for whoever next asks why the count
went to five.

`FAILED` is the wrong state for it: `FAILED` means the candidate cache was exhausted, and
it is revived on a schedule. A job in this situation has nothing to revive — the
candidate worked, there is no cache to retry against.

`NOT_IMPORTED` is the wrong state too, for the opposite reason. It is documented as
manual-jobs-only and as something that must never be retried or treated as an error. A
permanent refusal is neither of those things: it is resolvable — fix the tags, add the
release in Lidarr — so it has to stay retryable, and folding it into a state that is
defined as never-retried would have buried an actionable problem behind a label that
says nothing is wrong.

`PARKED` is half right. "Awaits a person's decision" fits exactly. "No candidate could
satisfy it" does not: the candidate was fine, and Lidarr said no anyway. Overloading
`PARKED` would make it mean two different failure shapes depending on which job you were
looking at.

## The name is REFUSED, not REJECTED

The event `import_rejected` already exists, and it is written every time a candidate is
rejected *and the job continues* — the opposite outcome from what this state means. One
canary job carries 59 `import_rejected` events and was never terminal; the pipeline just
moved on to the next candidate each time. Naming the new state `IMPORT_REJECTED` would
have put the same word in the same table meaning two opposite things, often on the same
job. `IMPORT_REFUSED` avoids that collision.

## No automatic revival

The state means "awaiting a person's decision." A timer that decides for them contradicts
that meaning, so unlike `FAILED` there is deliberately no schedule that moves a job out
of `IMPORT_REFUSED` on its own.

## Considered and rejected

**Reuse `FAILED`.** Wrong meaning (candidate exhaustion, not a Lidarr refusal) and wrong
behavior (revived on a schedule) — a permanently-refused-but-correct download would be
retried against Soulseek for no reason, since nothing about the candidate needs
replacing.

**Reuse `NOT_IMPORTED`.** `NOT_IMPORTED` is for a job that never reached Lidarr at all,
and is documented as never-retry, never-an-error. A permanent refusal *has* reached
Lidarr and *is* resolvable by a person. Folding it in would have either broken the
never-retry guarantee `NOT_IMPORTED` depends on elsewhere, or hidden a real, fixable
problem inside a state that says nothing is wrong.

**Reuse `PARKED`.** Only half the meaning matches — see above. Overloading it would make
`PARKED` describe two unrelated failure shapes, the same kind of ambiguity the
REFUSED/REJECTED naming choice exists to avoid.

## Accepted risk: Lidarr's `Permanent` flag is not always permanent

The producer decides a job has reached `IMPORT_REFUSED` by acting on Lidarr's `Permanent`
flag the first time a rejection carries it. But Lidarr's rejection constructor,
`Rejection(string reason, RejectionType type = RejectionType.Permanent)`, defaults to
`Permanent` — so a rejection whose underlying cause is actually transient still arrives
flagged permanent. This was verified against Lidarr's `develop` branch for two cases that
construct a `Rejection` with no explicit type: "File is still being unpacked"
(`NotUnpackingSpecification.cs`) and "Not enough free space" (`FreeSpaceSpecification.cs`).

"Not enough free space" is the more dangerous of the two because it is globally
correlated rather than per-job: one full disk refuses every job in flight at once, each
one landing in a state with no automatic revival. Recovery is a person clearing space and
then retrying every affected job by hand — there is no bulk un-refuse.

A reason denylist, a threshold that treats known-transient reasons differently, and
treating the `Permanent` flag as advisory rather than decisive were all considered. None
were implemented; the maintainer chose to treat the flag as decisive as shipped and watch
the canary. This is recorded as an accepted risk, not a solved problem — the mitigation
is a person noticing and retrying by hand, not a code-level safeguard.

## Consequences

- Terminal states go from four to five. A job can now stop in `IMPORT_REFUSED` with
  correct, complete files on disk and no automatic path back into the pipeline.
- A full disk on the Lidarr side can silently convert every in-flight job into
  `IMPORT_REFUSED` at once, because `FreeSpaceSpecification.cs` builds its rejection with
  the `Permanent` default. There is no code-level distinction between that and a genuine
  permanent refusal until this is revisited.
- Recovery from either the free-space case or a genuine permanent refusal is the same
  manual path: a person inspects the job, fixes whatever Lidarr objected to, and retries
  it themselves. Nothing retries `IMPORT_REFUSED` jobs automatically, by design.
