# A chronically failing peer is demoted by sort order, not by weight or filter

`matcher.Rank` scores each candidate by adding weighted terms and sorts on the total.
That shape cannot express "choose this only if there is nothing else", and issue #508 is
what happens when the system needs to. One peer sat at 31 successes against 657 failures
and kept being picked first, because it advertised complete FLAC sets and format plus
file count together outweigh everything history can say.

The fix is a second sort key. `matcher.IsLastResortPeer` cuts on the same
`ReliabilityHistoryScore` the `known_user` weight already uses, and `Rank` sorts every
candidate below the cut behind every candidate above it, whatever the scores. Inside each
tier the existing scoring and tie-breaks are untouched.

## Why the two obvious alternatives do not work

**Raising the `known_user` weight** does not generalise. `ReliabilityHistoryScore` is
normalised to 0..1, so at weight 1.0 the entire penalty is 1.0 point against a total that
spans about 4.3. To dominate the rest of the scale the weight would have to reach roughly
4 — and a weight that large does not switch on only for ruined peers. It would let
history drive the ranking between two peers that both work perfectly well, which is a
different and worse system. An additive term is a slope; the requirement is a step.

**Deleting the peer's `known_users` row** is not a penalty at all, it is a pardon.
`ReliabilityHistoryScore` returns 0.5 for a peer with no history, so deleting the row
would move the peer this issue was written about from 0.21 *up* to 0.50 and rank it
higher than before. It is also blocked in the schema: `artist_user_reliability.user_id`
carries a foreign key onto `known_users(id)` (migration 0001).

**A filter** was already rejected once, by #317, and for a reason that still holds: an
album whose only seeder is a bad peer becomes permanently unobtainable. The tier is
deliberately an ordering and never a filter. Discovery walks the ranked list and stops at
`MaxCandidates`, so a last-resort peer is dropped whenever better alternatives exist —
which is the intent — but it is still selected when it is all there is.
`TestDiscoveryEnqueuesASoleLastResortCandidate` exists to keep that true.

## Where the threshold can sit

`LastResortThreshold` is 0.25, and there is a floor beneath it that is easy to miss. The
peers this targets fail across many different albums, so they rarely have artist-scope
history and are judged on the global scope alone, which contributes at half weight with
its decayed count capped at 20. That bounds net history at -10, which over the sigmoid
scale of 5 is -2, and `sigmoid(-2)` is about 0.119. **A threshold at or below ~0.12 can
never fire for exactly the population the tier exists to catch.** (A peer with
artist-scope failures too can reach `sigmoid(-6)`, about 0.0025, but relying on that
would restrict the tier to repeat offenders on a single artist.)

0.25 is the lowest round value above the 0.21 scored by the motivating peer. On the
production data it selects 321 of 8369 known peers, or 3.8%.

One thing worth recording, because the issue's framing invites the wrong conclusion: the
31-against-657 ratio is *not* what condemns that peer. `ReliabilityCountCap` collapses
both sides to 20, so on counts alone it scores near neutral. What separates them is
recency — `decayedNet` ages successes and failures independently from their own
timestamps, and this peer's successes are stale while its failures are fresh. Anyone
tuning the threshold against raw success/fail ratios will get answers that do not match
what the ranker does.

The threshold is a compile-time constant, not a config key. Config rejects unknown keys
and has no silent defaults, so a new required key would stop every other self-hoster's
container on its next start.

## The tier decays, so a peer can heal

Because the cut is on the decayed score, a peer whose bad run ages out returns to the
normal tier on its own. Nothing needs to clear a flag or reset a counter, and there is no
state to become permanently wrong. `TestIsLastResortPeerFalseOnceAnOldRecordHasDecayed`
pins this.

## The flag on `candidates` is history, not a cache

Migration 0022 adds `candidates.last_resort`, written once at insert. It is never
recomputed from current peer history on read, because the display exists to answer a
historical question — why was this obviously bad peer chosen? — and the stored answer and
the recomputed one disagree exactly when the peer's record has since changed, which is
the case a reader is most likely to be looking at.

## The cooldown from #507 stays, and does not substitute for this

The two mechanisms are orthogonal and both remain. A cooldown governs *when* a peer may
be reconsidered; the tier governs *whether it wins* when it is. Seven days of production
data after #507 shipped is what motivated this: over twenty distinct peers sat at exactly
five escalations, having failed every rung of the 8h→16h→32h→64h→72h ladder back to back,
and normalised against total selection volume that cohort was being chosen 1.4x to 34x
*more* often per unit of work than before the cooldown existed. #507 killed the tight
loop; it did not make any peer lose a comparison.

## Forced search is not exempted

Issue #508 asked for the user-triggered forced search to skip the tier, on the analogy
that forced search already clears cooldowns. That was specified and then dropped, with
the maintainer's agreement.

The analogy does not carry. A cooldown is a time gate, so leaving it in place can make a
forced search silently do nothing — "try again later" is precisely what the button
overrides. The tier can never do that: it is an ordering, so a last-resort peer that is
the only candidate is still picked. Exempting forced search would therefore not unblock
anything. It would only put the chronically broken peer back at rank one for the single
job the user pressed the button on *because it was stuck*, which is the opposite of what
the button is for.

It also had a real cost. `ForceSearchJob` leaves no marker on the job — it sets
`state=WANTED, retries=0, not_before=NULL` and deletes the candidate rows — so Discovery
cannot tell a forced cycle from an ordinary one. Honouring the exemption would have meant
a new `album_jobs` column, a write, a read and a clear threaded through the pipeline, and
`Rank` would no longer be callable without knowing what triggered it.
