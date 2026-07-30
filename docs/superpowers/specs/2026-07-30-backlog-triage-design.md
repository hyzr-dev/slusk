# Backlog triage — a project-manager capability

Design, 2026-07-30. Status: approved, not yet implemented.

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

| File | Owns |
|---|---|
| `.claude/skills/backlog-triage/SKILL.md` | The method: the prod-impact rubric, the evidence contract, the rule that dependencies come from file overlap, browser serialization, the known-noise list, the report layout. Entry point `/backlog-triage`. |
| `.claude/workflows/backlog-triage.js` | The engine: fan-out, the barrier, wave computation. |

The split mirrors `verifying-ui-in-browser`, which is a contract rather than code. It also
matters mechanically: `Workflow` may be invoked when a skill's instructions say to, so the
skill is what makes `/backlog-triage` a legitimate entry point to orchestration instead of
something the user must opt into by hand every time.

## Phases

### 0. Inline scouting (orchestrator, not the script)

Workflow scripts have no filesystem access, so everything file-shaped is read inline and
handed in as `args`:

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

Failures are matched against the known-noise list carried by the skill, keyed by issue
number:

| Failure | Known as |
|---|---|
| `TestOpenRecyclesIdleConnections` (flaky under load) | #171 |
| `Settings.test.tsx` timeout under the full suite | #242 |
| `TestConnectPeerIndirectSuccess` in container | #250 |

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

- An edge joins two issues when their `touches` overlap at directory or file level.
- Overlap is deliberately coarse at directory level: `internal/pipeline/` is the only
  contact surface between modules, so two issues there are treated as conflicting even in
  different files. A false conflict costs one extra wave; a missed one costs a merge
  conflict inside a running agent's worktree.
- Greedy colouring yields wave 1 as the largest pairwise-disjoint set, ordered by
  `prodImpact` rank with `effort` ascending as tiebreak.
- `statedBlockers` are added as hard ordering edges on top.

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

## Output

`docs/triage/YYYY-MM-DD-backlog.md`, sections ordered by what they demand of the reader:

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
