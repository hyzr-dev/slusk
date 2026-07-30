---
name: backlog-triage
description: Use when the backlog needs a project manager's read — ranking every open issue against production stability, working out which can be implemented in parallel and which depend on each other, and checking what still reproduces. Analysis only; it never implements, branches or writes to Gitea.
---

# Backlog Triage

Read every open issue, rank it by what it does to the running instance, compute which
issues can be worked in parallel, and write a dated report. The report is the deliverable.

**This capability never implements anything.** No branches, no commits, no PRs, no Gitea
writes, no closing issues. Merging to `main` deploys within minutes, so the thing that
ranks work must not also ship it. It also never starts the PR lab.

## Procedure

### 1. Scout inline

Workflow scripts have no filesystem access, so read these yourself first:

```bash
tea issues --state open --output json
```

Read `docs/triage/state.json` if it exists. Then, using its `computedAt` SHA:

```bash
git diff --name-only <computedAt>..HEAD
```

Feed all three to the invalidation rule — do not reimplement it:

```bash
node scripts/triage/waves.mjs invalidate
```

on stdin as `{"state": ..., "openIssues": [{"number":..,"updated":..}], "changedPaths": [...]}`.

A cached judgement dies on either axis: the issue moved, or the code under it moved.
`touches` is an asserted relation between the two, so one axis is not enough — an issue can
sit untouched for months while the file it concerns is refactored away.

`invalidate` prints `{"fresh": [...], "stale": [...]}`. **Both arrays are bare issue
numbers, not objects.** Keep both:

- `stale` numbers become `args.issues` for the workflow in step 2 — these get re-judged.
- `fresh` numbers are not themselves what the workflow needs. Look each one up in
  `docs/triage/state.json`'s `issues` map (keyed by issue number as a string) and collect
  the full judgement *objects*. That list of objects is `args.cached` in step 2. The
  workflow trusts `args.cached` without validating it — it does `[...cached, ...judged]`
  directly into the report's judgement list — so only put judgement objects from the state
  file there, nothing else. If you pass the bare `fresh` numbers instead of the objects
  they name, the wave computation in step 3 gets numbers where it expects judgements and
  produces nothing useful.

If there is no `docs/triage/state.json` yet (first run), everything is stale: pass every
open issue number as `args.issues` and `args.cached: []`.

### 2. Run the workflow

Invoke the Workflow tool with `{scriptPath: ".claude/workflows/backlog-triage.js", args:
{issues: <stale numbers>, cached: <fresh judgement objects, per step 1>}}`.

The script runs the baseline first and aborts if `main` is red on anything that is not
known noise. Its return value carries `aborted` as one of five distinct strings — never a
boolean — and each means something different:

| `aborted` value | What actually happened | What to say in the report |
|---|---|---|
| `'baseline-agent-died'` | The baseline agent itself returned nothing | The repo's state is **unknown** — the check that would have told you never ran. Do not call this a red baseline. |
| `'suite-red'` | `unknownFailures` was non-empty — a failure not on CLAUDE.md's known-noise list | `main` is red; name the failures |
| `'go-red'` | `go test ./...` did not report green | `go test ./...` is red |
| `'web-red'` | `cd web && npm test` did not report green | the web suite is red |
| `'triage-red'` | `node --test scripts/triage/*.test.mjs` did not report green | the triage suite's own tests are red |

(The baseline agent runs the suite as three separate legs — `go test ./...`, `node --test
scripts/triage/*.test.mjs`, `cd web && npm test` — because the bare-directory form `node
--test scripts/triage/` fails on this machine's Node; the workflow script already gets this
right, this is only relevant if you are explaining an abort to a human.)

If `aborted` is set at all: report which of the five happened, in those terms, and stop.
Do not compute waves, do not write the dated report body past the header, do not write
`state.json` — there is nothing new to cache. Ranking issues against a baseline that either
failed or was never checked is ranking fiction either way.

The return value's `unassessed` field is the list of issue numbers whose judge agent
returned nothing at all — the issue was never assessed. Read it directly off the result
(`result.unassessed`), alongside `judgements`, `baseline` and `browser`. It is always an
array, including on every abort path, so there is nothing to test for before reading it.
Do not go looking for it in the workflow's log output — a value the script already computed
belongs in the return value, not in something meant for a human watching the run.

**Before caching a judgement, stitch on the issue's `updated` timestamp.** The workflow's
judgement schema has no `updated` field — it never sees the tea issue JSON, only the
collector's distillate — but `waves.mjs invalidate` compares `cached.updated !== open.updated`
to detect a moved issue. Whichever judgement objects you write into `docs/triage/state.json`
(step 4) must carry the `updated` value from the tea issues JSON you fetched in step 1, keyed
by the same issue number, or every cached judgement will look permanently stale (or
permanently fresh, if you fabricate a constant) on the next run.

### 3. Compute the waves

```bash
node scripts/triage/waves.mjs waves
```

with `{"issues": <all judgements — cached and newly judged>, "contracts": <the "contracts"
array from scripts/triage/contracts.json, not the whole file>}`.

**No agent decides what runs in parallel.** Two kinds of edge: file overlap, and shared
contracts. The second exists because file overlap is blind to coupling across a wire — two
issues can change opposite ends of one protocol while touching disjoint files, and that
skew is silent and green, because each side is tested against its own mock.

If a contract is missing from `contracts.json`, the waves cannot see it. Add it there when
you find one.

This also returns `unassessable`: issue numbers whose `prodImpact` or `effort` was not one
of the recognised values (a typo, an omitted field, anything else `isAssessable()` rejects).
These issues are excluded from every wave — they never got ranked, they did not rank last.
Keep this list; it goes in the report and in `state.json` (step 4).

### 4. Write the report

`docs/triage/YYYY-MM-DD-backlog.md`, sections in this order — what demands a human first:

1. **Header** — baseline result (or which of the five `aborted` values fired), commit,
   judged vs cached counts, browser coverage.
2. **Requires your decision** — every issue with a `needsDecision` flag. First, because it
   is the only part that blocks. A new required config key found after a merge stops the
   container on the next deploy; found here it is a line read in ten seconds.
3. **Waves** — bare issue numbers, e.g. `` `#272 #212 #286` ``. Readable by a human and
   pasteable into an implementation run.
4. **Dependencies** — a mermaid graph of the conflict and blocker edges.
5. **Full judgement** — a table: issue, impact, evidence, effort, repro status, touches.
6. **Unassessed** — the `unassessed` issue numbers from step 2's result: the judge agent for
   that issue returned nothing, so it was never assessed at all. This is an infrastructure
   failure worth retrying, not a judgement call — say that plainly.
7. **Unassessable** — the `unassessable` issue numbers from step 3: a judgement came back,
   but its `prodImpact` or `effort` was not a recognised value, so the wave computation
   excluded it. This is a bad judgement worth reading, a different failure from Unassessed —
   keep the two sections separate so a reader can tell which happened.
8. **Candidates to close** — what was observed. Not a conclusion: "I could not reproduce
   it" and "it is fixed" are different claims, and the second is the maintainer's.
9. **Not verified (BLOCKED)** — with the reason and the command that would unblock it.

Then write `docs/triage/state.json` as:

```json
{
  "computedAt": "<HEAD sha>",
  "issues": { "<number>": "<judgement, with its updated timestamp stitched on>" },
  "waves": [["<numbers>"]],
  "unassessable": ["<numbers>"],
  "unassessed": ["<numbers>"]
}
```

`issues` holds every judgement this run knows about — reused and newly judged — keyed by
issue number as a string, so next run's `invalidate` step can find them. `waves` is what
makes the report checkable after the fact. `unassessable` (from step 3's result) and
`unassessed` (from step 2's result) are kept apart deliberately: one is a judgement with a
value the scheduler didn't recognise, the other is no judgement at all. Collapsing them
would hide which failure mode actually happened.

### 5. Reap what the run left behind

The script cannot do this — it has no shell. After any run that started a Vite server or a
test runner:

```bash
ps -eo pid,ppid,rss,etime,command | grep -Ei 'vitest|jest|node .*vite' | grep -v grep
```

Kill the rows with `ppid=1` older than the run. Seven orphaned vitest workers have been
measured at 4.8 GB.

## Shell Independence

Every command here must run the same under bash, zsh and fish. There is no dialect all
three accept, so **name the interpreter**: anything using command substitution, a loop, a
heredoc, an environment prefix or an exit-status variable runs as `bash -c '...'`, or from
a file via `sh file.sh`. Plain single commands need no wrapper.

`$(cmd)` works in fish 3.4+. What does not: `VAR=value cmd` (use `env VAR=value cmd`), `$?`
(it is `$status`), `$PIPESTATUS` (it is `$pipestatus`, as in zsh), `for ...; do ...; done`,
and heredocs, which fish does not support at all.

## Common Mistakes

| Mistake | Fix |
|---|---|
| Judged an issue from its text | Read the code it points at; a summary is not a judgement |
| An agent decided the waves | The waves come from `waves.mjs`; agents supply `touches`, not scheduling |
| Ranked issues against a red baseline | The script aborts for a reason — report which `aborted` value fired and stop |
| Treated `aborted: 'baseline-agent-died'` as "the repo is broken" | It means the check never ran, not that it failed — say that, don't conflate the two |
| Passed `fresh` issue numbers as `args.cached` | The workflow needs judgement *objects*; look each `fresh` number up in `state.json` first |
| Went looking for `unassessed` in the log output | It's a field on the return value (`result.unassessed`), always an array; read it directly |
| Wrote judgements to `state.json` without an `updated` timestamp | Next run's `invalidate` needs it on every cached entry, or cache freshness breaks |
| Merged `unassessable` and `unassessed` into one bucket | They are different failure modes; keep them apart in the report and in `state.json` |
| Called a browser check PASS without rendering | That is BLOCKED; `verifying-ui-in-browser` owns the contract |
| Two browser verifiers at once | Playwright MCP owns one browser — the loop is serial on purpose |
| Started the lab to unblock a check | Never; report BLOCKED and print `./testenv/lab.sh up` |
| Closed an issue that did not reproduce | It becomes a candidate to close; the decision is the maintainer's |
| Silently verified only the top few | Cap is fine, silence is not — `log()` what was dropped |
| Left a stale entry in the noise list | An entry pointing at a closed issue hides a real failure; flag it |
