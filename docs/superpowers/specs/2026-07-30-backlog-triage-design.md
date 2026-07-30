# Backlog triage — a project-manager capability

Design, 2026-07-30. Status: implemented. See the amendments at the foot of this document —
the implementation diverged from this design in six places, and the shipped code is the
authority wherever they disagree. Amendments are recorded rather than edited in silently, so
that the reason a decision changed survives alongside the decision.

## Purpose

Read every open Gitea issue, judge its priority against production stability, work out
which issues can be implemented in parallel and which depend on each other, and ground
the judgement in observation rather than in what the issue text claimed when it was
written. The output is a report a human acts on.

At 30 open issues the backlog no longer fits in one reading, and the issues that matter
most are not the ones most recently filed.

## Non-goals

- **It does not implement anything.** No branches, no commits, no PRs. Merging to `main`
  deploys to production within minutes; a capability that ranks work must not also be a
  capability that ships it.
- **It does not write to Gitea.** No comments, no label changes. A misjudgement stays
  local and costs nothing to discard.
- **It does not close issues.** It may report that a defect could not be reproduced.
  Concluding that it is fixed is the maintainer's call.
- **It does not start the PR lab.** Starting `testenv/` logs into a real Soulseek account,
  takes minutes, and only one lab can run at a time.

## Two files

> **Amended — see [A3](#a3-four-parts-not-two-files).** There are four parts, not two: wave
> computation moved out of the workflow script into `scripts/triage/waves.mjs`, with its data
> in `scripts/triage/contracts.json`.

| File | Owns |
|---|---|
| `.claude/skills/backlog-triage/SKILL.md` | The method: the prod-impact rubric, the evidence contract, the rule that dependencies come from file overlap, browser serialization, the known-noise list, the report layout. Entry point `/backlog-triage`. |
| `.claude/workflows/backlog-triage.js` | The engine: fan-out, the barrier, wave computation. |

The split mirrors `verifying-ui-in-browser`, which is a contract rather than code. It also
matters mechanically: `Workflow` may be invoked when a skill's instructions say to, so the
skill is what makes `/backlog-triage` a legitimate entry point to orchestration instead of
something the user must opt into by hand every time.

## Relationship to the existing skills

The rule for every agent in this design: **delegate to a skill when a skill owns the
contract; write the prompt inline when the stage is bespoke.** A skill exists to carry a
contract across several call sites; a stage with one call site does not need one.

| Skill | Used how |
|---|---|
| `verifying-ui-in-browser` | Required sub-skill for phase 2's browser leg. It owns the three-verdict contract and the one-browser rule. Restating it here would create a second truth. |
| `issue-tracker-cli` | Required sub-skill wherever `tea` is called — phase 0 and 1a. Extracted from the four skills that had each grown their own copy. |
| `resolve-issue` | **Not used.** See below. |
| phases 1a, 1b, 3 | Inline prompts. No skill owns "distil issue JSON" or the prod-impact rubric, and the rubric lives in this design's own `SKILL.md`. |

`resolve-issue` cannot run as a subagent. It has two hard human gates — "wait for user
approval before implementing" in step 3, and "wait for the user to confirm the fix works"
before the PR in step 7. A subagent invoking it blocks on a user who is not there. It is
an orchestrator script for a session with a human present, not an agent prompt.

It is still worth reading before implementing this. Its model table (haiku to explore,
sonnet to implement and fix, the default model to plan and review) is close to the one
below, so the budget rule here follows established practice rather than inventing it. Its
pre- and post-flight branch checks exist because an agent's commit once landed on the
user's branch — irrelevant to a capability that never commits, but the reason to keep
that skill as the model for anything that does.

One asymmetry is worth stating because it justifies a field in the schema. `resolve-issue`
decides whether browser verification is needed by running a predicate on the diff, and
says explicitly not to judge from the issue title. This design has no diff — nothing has
been implemented — so its selection rests on `frontend` as judged in 1b, and is
principally weaker. That is why `reproCheck` must be a concrete falsifiable claim rather
than a boolean: it is what compensates for the missing predicate.

## Phases

### 0. Inline scouting (orchestrator, not the script)

Workflow scripts have no filesystem access, so everything file-shaped is read inline and
handed in as `args`:

> **Amended — see [A1](#a1-invalidation-is-a-content-digest-not-a-timestamp) and
> [A2](#a2-tea-issues-truncates-silently-at-30).** There is no `updated` field in `tea`'s
> output, so the first axis is a content digest; and the `tea` invocation below needs an
> explicit `--limit` or it silently returns only the first 30 of 42 issues.

1. `tea issues --state open --output json` — numbers and `updated` timestamps.
2. `docs/triage/state.json` — the previous run's structured judgements.
3. `git diff --name-only <state.computedAt>..HEAD` — what has moved in the code.

A cached judgement is invalidated on either of two independent axes:

- **the issue moved** — `updated` is newer than the cached entry, or the issue is new;
- **the code under it moved** — the cached `touches` intersects the diff.

Both are needed because `touches` is an asserted relation between an issue and the code.
An issue can sit untouched for months while the file it concerns is refactored away.

`computedAt` is a commit SHA, not a timestamp: scripts may not call `Date.now()` (it would
break resume caching), and a SHA is what we actually want to diff against.

### 1a. Collect (parallel, one agent per invalidated issue)

Mechanical only: fetch `tea issues <n> --comments --output json`, extract title, labels,
comment substance, and any file paths the text names.

Returns `thin: true` when the comment thread was long and technical, signalling that the
distillation may have dropped something load-bearing.

### 1b. Judge (parallel, one agent per invalidated issue)

Reads the code that 1a pointed at. **Reading the issue text alone produces a summary, not
a judgement** — this is the rule the whole design rests on. When 1a set `thin`, this stage
fetches the raw JSON itself.

```js
{ number, kind: 'bug'|'feature'|'techdebt'|'test',
  prodImpact: 'none'|'cosmetic'|'degraded'|'dataloss'|'outage',
  impactEvidence: "file:line where it manifests",
  touches: ["internal/pipeline/importing.go", "web/src/views/Jobs.tsx"],
  frontend: bool,
  effort: 'S'|'M'|'L',
  reproCheck: "route or command that decides whether it still holds" | null,
  concurrency: bool,                    // whether -race is worth running for it
  statedBlockers: [274],                // ONLY when the issue text says so
  needsDecision: { configKey, migration, architecture } }
```

### 2. Verify (serial)

**Baseline, once — not per issue.** `go test ./...` and `npm test` against the working
tree. `-race` is not part of the baseline; it is too slow over ~717 tests to pay for
itself here, and runs only for issues 1b flagged `concurrency`.

Failures are matched against the known-noise list, which lives in the repo's `CLAUDE.md`
rather than in the skill. That is deliberate: `CLAUDE.md` is loaded for every agent in the
repo, so the same list also serves an implementer running the suite, and there is only one
copy to keep true.

The list must state *where* each failure is visible, not just that it exists. Verifying it
on 2026-07-30 showed why: #171 passes a plain `go test ./...` and only fails under
`-count=5`, so agent runs had been reporting green while the defect was still there — and
#242, listed on the same reasoning, turned out not to reproduce at all and left the list.

> A failure that is not on that list is not a backlog entry — it means `main` is red.
> Triage aborts and the report consists of that single finding.

Prioritising thirty issues against a broken baseline is prioritising fiction.

The list is keyed on issue numbers rather than test names so the run can notice that a
noise entry points at a closed issue and flag the list as stale. It is the only part of
this design that must be maintained by hand, and it rots silently.

**Browser reproduction, serial.** Selection is `frontend && reproCheck != null &&
kind != 'feature'` — a feature has nothing to reproduce. Each selected issue is verified
by following `verifying-ui-in-browser` exactly, which already handles free-port
allocation, serving from the right path, and the three-verdict contract.

Serialization is not a choice: the Playwright MCP server owns a single browser instance
per session, and two concurrent verifiers return verdicts about each other's tab.

The lab is probed, never started: `curl -sf localhost:9090/status`. If it does not answer,
every browser item is `BLOCKED`, and the report names the unverified issues and prints
`./testenv/lab.sh up`. Grading contrast by reading the CSS instead is exactly what
`BLOCKED` exists to prevent.

The browser phase is capped at the four highest-ranked frontend issues. What was dropped
is `log()`ed — a silent cap reads as full coverage.

### 3. Synthesize

Waves are computed in plain JavaScript. **No agent decides what can run in parallel.**

Two kinds of edge, both computed, neither judged:

**File overlap.** An edge joins two issues whose `touches` intersect at file level.

**Shared contract.** File overlap is blind to coupling across a wire: two issues can change
opposite ends of the same protocol while touching entirely disjoint files. #275 (the stream
must carry page invalidation) changes the producer in `internal/observ/stream.go`; #267 (two
EventSource connections on mount) changes the consumer in `web/src/api/stream.tsx`. Zero
overlap, so the algorithm would schedule them together — and two agents would change
opposite ends of one protocol at the same time. That is worse than a merge conflict: a
merge conflict is loud, a protocol skew is silent and green, because each side is tested
against its own mock.

So the design carries a short, explicit list of shared contracts with both their sides:

| Contract | Side A | Side B |
|---|---|---|
| `sse-events` | `internal/observ/stream.go` | `web/src/api/stream.tsx` |
| `album-jobs` | `internal/store` schema | `internal/pipeline` |
| `config` | `internal/config` | `config.example.toml` and production's `config.toml` |

> **Deviation, deliberately not fixed — see
> [A6](#a6-contractsjsons-album-jobs-side-b-is-a-directory-and-stays-one).** The shipped
> `scripts/triage/contracts.json` gives `album-jobs` side B as `internal/pipeline/`, a
> package-level side that reinstates exactly the coarse directory rule this section says was
> replaced. This design is right and the data is over-conservative; it was left alone because
> it costs extra waves, not correctness.

An issue touches a contract when its `touches` intersects either side. **Two issues
touching the same contract conflict regardless of file overlap.**

This replaces an earlier rule that treated all of `internal/pipeline/` as one unit on the
grounds that it was the only contact surface between modules. That premise came from
CLAUDE.md and is no longer true — the SSE layer is a second one, tracked as #290 along with
three other places the file has drifted from the code — and the coarse directory rule
turned out to be a poor approximation of "shared contract" anyway. With contracts
modelled explicitly, file-level granularity is enough everywhere, which is both less
conservative and more correct.

The list must be maintained by hand, like the known-noise list, and it is worth stating
plainly that a contract nobody records is a contract the waves cannot see.

Then: greedy colouring yields wave 1 as the largest pairwise-disjoint set, ordered by
`prodImpact` rank with `effort` ascending as tiebreak. `statedBlockers` are added as hard
ordering edges on top.

Only then does one agent write the prose. It explains the computed structure; it does not
decide it.

## Model budget

In a workflow script the default is the trap: an omitted `model` inherits the session
model, so a 30-way fan-out without one is 30 Opus agents.

> **Every `agent()` call in this script names `model` explicitly. An omitted `model` is a
> bug here, not a default.**

| Stage | Model | `effort` | Why |
|---|---|---|---|
| 1a Collect | `haiku` | `low` | Objective extraction, no judgement. Keeps raw `tea` JSON — long threads on #106, #215 — away from an expensive model. |
| 1b Judge | `sonnet` | default | Reads code, assigns impact with evidence. Judgement. |
| 2 Verify | `sonnet` | default | Follows a procedure to a three-value verdict. |
| 3 Synthesize | session model | `high` | One call, sees the whole backlog, writes the reasoning. |

Model choice saves per call; not loading the artefact saves per observation. The browser
stage therefore passes `filename` to `browser_snapshot` and greps the tree from disk
rather than pulling a whole dashboard's accessibility tree into context.

## Shell independence

Every shell command this skill issues must run the same under bash, zsh and fish. The rule
is not to find a dialect all three accept — there isn't one — but to **name the
interpreter**:

> Anything using command substitution, a loop, a heredoc, an environment prefix or an
> exit-status variable is run as `bash -c '...'`, or written to a file and run with
> `sh file.sh`. The ambient shell then sees a single word and its dialect is irrelevant.

Plain single commands (`tea issues 42 --output json`, `go test ./...`) need no wrapper.

What actually differs, and what does not:

| Construct | fish |
|---|---|
| `$(cmd)` | Works since fish 3.4 — not the problem it is assumed to be |
| `VAR=value cmd` | Unsupported. `env VAR=value cmd` works everywhere |
| `$?` | Spelled `$status` |
| `$PIPESTATUS` | Spelled `$pipestatus` — as in zsh; only bash uppercases it |
| `for x in ...; do ...; done` | Different syntax; fish ends with `end` |
| `<<'EOF'` heredoc | Unsupported entirely |

This was written after assuming bash inside a zsh session: `${PIPESTATUS[0]}` expanded to
nothing, and a test run reported an empty exit code that looked like a pass. A wrong shell
assumption does not usually fail loudly — it produces a plausible wrong answer, which is
the same failure mode the rest of this design exists to prevent.

**Known limitation.** Phase 2 delegates to `verifying-ui-in-browser`, which currently uses
an environment prefix and a `do/done` loop. Until that skill is updated, the browser leg is
bash-dependent regardless of what this one does. It is not blocking — the triage runs the
browser stage through a subagent whose own shell is bash — but the claim "shell
independent" applies to this skill's commands, not to the whole chain.

## Output

`docs/triage/YYYY-MM-DD-backlog.md`, sections ordered by what they demand of the reader:

> **Amended — see [A4](#a4-the-report-has-twelve-sections-not-seven).** The shipped layout has
> twelve sections. `SKILL.md` is the authority on the report layout; this list is the original
> sketch.

1. **Header** — baseline result, commit, issues read vs. cached, browser coverage.
2. **Requires your decision** — the `needsDecision` flags. First, because it is the only
   part that blocks. A new required config key discovered after a merge stops the
   container on the next deploy; discovered here it is a line read in ten seconds.
3. **Waves** — bare issue numbers, e.g. `` `#272 #212 #286 #180` ``. Readable by a human
   and pasteable into an implementation run as `args: { issues: [...] }`. The report is
   the interface between analysis and execution, without the triage ever dispatching.
4. **Dependencies** — mermaid graph.
5. **Full judgement** — table: issue, impact, evidence, effort, repro status, touches.
6. **Candidates to close** — what was observed, not a conclusion.
7. **Not verified (BLOCKED)** — with the reason.

Dated filename: two runs the same day overwrite each other, while `git log docs/triage/`
becomes a history of how the backlog moved. Together with `state.json` — also committed,
so a diff shows when the triage changed its mind about an issue — it answers "when did
this become urgent, and why".

> **Amended — see [A5](#a5-the-capability-commits-nothing-including-its-own-output).** The
> capability never commits anything, its own output included. Both files are left unstaged;
> committing them is the maintainer's decision, taken outside the capability.

## Error handling

| Situation | Behaviour |
|---|---|
| `main` is red on an unknown failure | Abort. Report the failure only. |
| Lab not reachable | Browser items `BLOCKED`, named in the report, `lab.sh up` printed. |
| A noise entry points at a closed issue | Report the list as stale. |
| `state.json` missing or unparseable | Full run, no cache. Not an error. |
| An agent returns null | Drop that issue from the waves and list it as unassessed. Never silently. |
| `touches` empty | Treated as conflicting with everything: it lands in its own wave. |

## Verification

There is nothing here to unit-test: the deliverable is a skill and an orchestration
script, and its output is a judgement. Verification is a dry run against the real backlog,
checked on four points:

1. The waves contain no pair with overlapping `touches` — mechanical, checkable from
   `state.json`.
2. Every `prodImpact` above `cosmetic` carries an `impactEvidence` that resolves to a real
   `file:line`.
3. A second run with no changes reuses the cache and produces a byte-identical report.
4. Deliberately touching a file named in a cached `touches` invalidates exactly that
   issue.

Point 3 is the one that matters: a triage whose output drifts between identical runs
cannot be diffed, and being diffable is most of the value.

After any run that started a Vite server or ran vitest, the orchestrator reaps orphaned
worker pools with `ps`. The script cannot: it has no shell.

> **Amended.** "There is nothing here to unit-test" did not survive implementation. Wave
> computation moved into `scripts/triage/waves.mjs` precisely so it could be tested, and it
> has 35 tests. The four dry-run checks above still stand as the end-to-end verification.

## Amendments

Recorded 2026-07-30, after implementation and a whole-branch review. Each entry says what
this design asserts, what the code actually does, and which is authoritative. **The code is
authoritative in A1–A5** — those are places the design was written before something was
known, and it was not edited in place because the reasoning that produced the original text
is worth keeping next to the correction. **A6 is the one entry where this design is right and
the shipped data is not**, and it says why the data was left alone anyway.

### A1. Invalidation is a content digest, not a timestamp

Phase 0 describes invalidating a cached judgement when the issue's `updated` timestamp is
newer than the cached entry. **There is no `updated` field anywhere in `tea`'s output.** The
issue list gives author, index, labels, milestone, owner, repo, state and title; a single
issue adds `created` and `closedAt`. Nothing records when an issue last changed, so there is
nothing to compare.

The implementation hashes the issue's **title, body, labels and comments** into a sha256
content digest and compares digests instead. Comments are in the digest deliberately: a
comment can add a `touches` path or change the severity picture as much as an edit to the
body, so hashing only the body would miss real change. The digest must contain nothing
time-based — a fetch timestamp or a salt makes every entry permanently stale and the cache
never hits.

The two-axis structure the design argues for is unchanged and still correct; only the first
axis's mechanism differs. `invalidate()` in `scripts/triage/waves.mjs` is the implementation;
`SKILL.md` step 1 is the procedure.

### A2. `tea issues` truncates silently at 30

Phase 0 shows `tea issues --state open --output json` with no `--limit`. That returns the
first page — 30 items by default — with no warning anywhere in its output. The real backlog
was 42 at the time of the first run, so this design's own command would have read 30 of them
and produced a report that looked complete.

The implementation passes `--limit 200` and treats a returned count landing exactly on a page
size as suspicious rather than as a coincidence. A capability whose premise is "every open
issue" and which silently covers page one only is worse than useless: it looks finished.

### A3. Four parts, not "Two files"

The design's file table has two entries and gives wave computation to the workflow script.
The implementation has four parts, because a Workflow script cannot be unit-tested and wave
computation is the one part of this capability where a wrong answer costs a merge conflict
inside a running agent's worktree:

| Part | Owns | Tested |
|---|---|---|
| `.claude/skills/backlog-triage/SKILL.md` | The method, the prod-impact rubric, the evidence contract, the report layout. Entry point `/backlog-triage`. | Prose; no |
| `.claude/workflows/backlog-triage.js` | The engine: arg validation, the baseline barrier, agent fan-out, the judgement schema, the serial browser loop. | No — a Workflow script cannot be imported |
| `scripts/triage/waves.mjs` | Pure computation: conflicts, contracts, rank, waves, cache invalidation. | Yes — 35 tests |
| `scripts/triage/contracts.json` | The shared-contract data the wave computation reads. | Data |

The split is the whole reason the scheduling rules are testable at all, so it is a departure
worth keeping rather than one to reconcile back.

### A4. The report has twelve sections, not seven

The Output section lists seven. `SKILL.md` — which is the authority on the report layout —
now specifies twelve. The five additions, in the order they appear:

- **Confirmed still reproducing** — the `ISSUES_FOUND` browser verdicts. The design's layout
  had `PASS` feeding Candidates to close and `BLOCKED` feeding Not verified, and no home for
  the third verdict, which is the most valuable thing the browser phase can produce.
- **Conflict density**, split out from what this design called simply "Dependencies". A
  conflict ("do not work these two at once") and a dependency ("do this one first") are
  different relations with different shapes; one graph carrying both leaves a reader unable to
  tell which an edge means, and a real backlog's conflict count — hundreds of edges across a
  few dozen issues — is unreadable as a graph anyway. Dependencies stay a mermaid graph of
  `statedBlockers` only; conflicts are a count plus the few issues driving most of them.
- **Unassessed** — the judge agent returned nothing.
- **Unassessable** — a judgement came back with an unrecognised `prodImpact` or `effort`.
- **Unschedulable** — a judgement whose `touches` named a directory (see
  [A6](#a6-contractsjsons-album-jobs-side-b-is-a-directory-and-stays-one) for the related
  data question). Three distinct exclusion modes needing three distinct repairs; this design's
  error-handling table had only "an agent returns null".

`SKILL.md` also adds a rule this design does not state: every list of issues in the report
declares its own sort order. A fresh run's judgements arrive in fetch order while a
regeneration reads `state.json`'s `issues` map, whose stringified-number keys JavaScript
always enumerates in ascending numeric order — two sources, two orders, same data, and a
section that iterates whichever it was handed makes successive reports undiffable.

### A5. The capability commits nothing, including its own output

The Output section says `state.json` is "also committed, so a diff shows when the triage
changed its mind about an issue". `SKILL.md` now forbids the capability committing anything
at all, its own two output files included, and that supersedes this.

The reasoning: an agent that commits its own output has decided, on the maintainer's behalf,
that this run's judgement is worth keeping — and a triage that ran on a bad premise or against
a red tree should be discardable without a revert. The consequence is that
`git log docs/triage/` is a history of the runs the maintainer chose to keep, not of every run
the capability produced. The design's "when did this become urgent, and why" still works over
the runs that were kept; it is just a weaker claim than the original text implies.

The one report currently in this repository's history was committed by hand, as verification
evidence for the plan that built the capability. Finding it in `git log` is not evidence that
committing is normal behaviour.

### A6. `contracts.json`'s `album-jobs` side B is a directory, and stays one

This is the one entry where the design is right and the data is not.

The Synthesize section argues at length that treating all of `internal/pipeline/` as one unit
was a poor approximation of "shared contract", and that with contracts modelled explicitly,
file-level granularity is enough everywhere. The shipped `scripts/triage/contracts.json`
nonetheless gives `album-jobs` two sides of `internal/store/migrations/` and
`internal/pipeline/` — both directories. The second reinstates precisely the coarse rule this
design replaced: every pair of issues touching any file under `internal/pipeline/` now
conflicts through the contract even when their files are disjoint.

**It was left unchanged deliberately.** The consequence is extra waves, not a wrong answer:
an over-conservative edge serialises work that could have run in parallel, which costs
throughput. The failure it would introduce if removed carelessly is the silent one — two
agents skewing opposite ends of the job-state contract, each green against its own mock.
Tightening it needs the file-level analysis of who actually writes `album_jobs` transitions,
which is real work with no test to catch a mistake in it, and it is not part of a fix wave.

The `migrations/` side, by contrast, earns its keep as a directory and should not be narrowed:
**two issues each proposing a new `0009_*.sql` under different filenames have no file overlap
at all.** The directory side is the only thing that stops two competing migration 0009s
sharing a wave — and since a merged migration is immutable in this repo, that collision is
expensive to unwind. So "directory sides are always over-conservative" is not the right
generalisation either; a side needs to be as coarse as the thing that actually collides, and
for migrations the thing that collides is the sequence number, not the filename.
