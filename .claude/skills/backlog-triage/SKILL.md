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

## Production-impact rubric

`prodImpact` is the ranking axis — `rank = impact * 10 - effort`, so effort is only a
tiebreak within one impact level. Every judge agent must use these five definitions and no
others. This section is the single copy; nothing else in this capability restates it.

| `prodImpact` | The running instance |
|---|---|
| `outage` | Stops doing its job: it will not start, the pipeline stalls entirely, or a component wedges permanently with no recovery. |
| `dataloss` | Data is destroyed, corrupted, or permanently stranded. Includes files left behind that nothing will ever clean up, jobs that can never leave a state, and unbounded growth that will eventually take the instance down. |
| `degraded` | Works, but measurably worse: wasted work, repeated retries, slow queries under real load, or missing observability that hides a genuine failure. |
| `cosmetic` | Every behaviour is correct; something the UI or the logs *says* is wrong, misleading or ugly. |
| `none` | No runtime effect at all: documentation, tests, or a refactor with no behavioural change. |

**The tie-breaking principle: judge the worst outcome the defect can produce in production
as the code stands today.** Not the most likely outcome, and not the outcome after some
other issue lands. This is the sentence to read twice, because it is exactly where privately
invented rubrics diverge — one judge reads "unbounded row growth" as `degraded` because the
table is small today, another as `dataloss` because nothing bounds it. The second is
correct. If an issue's severity depends on unmerged work, judge it as it stands and say so
in `impactEvidence`.

**"Outcome" means what the system does, not what a human might do in response to what it
displays.** A defect that only misinforms a reader is `cosmetic` however badly it misinforms
them — an operator acting on a wrong number is not the system behaving worse — and it becomes
`degraded` only when something in the system itself behaves worse as a result. Without that
bound, every misleading-UI defect can be argued up to `degraded` via a hypothetical operator,
which is exactly the seam between `cosmetic`'s "every behaviour is correct" and a maximal
reading of the worst-outcome rule.

**The first run's distribution is not a baseline.** The run recorded in
`docs/triage/2026-07-30-backlog.md` predates this rubric: 42 judges each invented the
boundaries privately and came out 38 of 42 at `cosmetic` or `none`, 4 at `degraded`, and
zero at `dataloss` or `outage`. An axis that flat collapses the ranking into the effort
tiebreak, which is the one thing this capability exists not to do. Do not calibrate against
those numbers, and do not treat a later run that ranks more issues above `degraded` as
inflation on that evidence alone.

Two judgements from that run are worth reading as examples of a boundary that is genuinely
hard, not as corrections to apply now:

- **#186** — any Soulseek peer can write unbounded rows to Postgres, with pruning ruled out
  in a migration comment. Unbounded growth with no bound anywhere is the `dataloss` row's
  own wording.
- **#266** — a broadcaster goroutine that can wedge forever on an untimed context. "Wedges
  permanently with no recovery" is the `outage` row's own wording.

Both were judged lower. Whether that was wrong is a judgement call; that it was made without
a written rubric to be wrong against is not.

## Procedure

### 1. Scout inline

Workflow scripts have no filesystem access, so read these yourself first.

**Fetch every open issue — watch for silent truncation.** `tea issues --output json`
truncates to a page (30 items by default) without saying so anywhere in its output. Pass an
explicit high limit:

```bash
tea issues --state open --limit 200 --output json
```

If the returned count is exactly a round page size (30, 50, ...), treat that as suspicious —
a real backlog landing on exactly a page boundary is far less likely than a silent
truncation — and re-run with a higher `--limit` before trusting the count. A capability whose
whole premise is "every open issue" that silently covers page one only is worse than useless:
it looks complete.

**There is no last-changed timestamp anywhere in `tea`'s output.** The issue list gives
author, index, labels, milestone, owner, repo, state, title; a single issue adds `created`
and `closedAt`. Nothing says when an issue last changed, so cache invalidation cannot compare
"did the issue move" by timestamp — there is nothing to compare. Use a content digest
instead. For every open issue, fetch its full content and hash it:

```bash
tea issues <n> --comments --output json > /tmp/issue-<n>.json
node -e '
const fs = require("fs")
const crypto = require("crypto")
const issue = JSON.parse(fs.readFileSync(process.argv[1], "utf8"))
const content = JSON.stringify({
  title: issue.title,
  body: issue.body,
  labels: issue.labels,
  comments: issue.comments,
})
console.log(crypto.createHash("sha256").update(content).digest("hex"))
' /tmp/issue-<n>.json
```

The digest must cover **title, body, labels and comments** — a comment can add a `touches`
path or change severity just as much as an edit to the body, so hashing only the body would
miss real changes. Do not add anything time-based (a fetch timestamp, a random salt) to what
gets hashed: the same content must produce the same digest on every run, or the cache never
hits. `node` is already a dependency of this repo's toolchain, which is why this one-liner is
the portable choice over `shasum`/`sha256sum` — those differ across platforms and this must
not.

This costs one cheap `tea` call and one cheap `node` invocation per issue — shell calls, not
agents — and that is deliberate: hashing 42 issues is far cheaper than judging them, which is
the entire reason a cache exists. Build `openIssues` as `[{ "number": n, "digest": "<hex>" },
...]`. The digest is what `invalidate` now compares in place of a timestamp; its stdin shape
still has the same three keys — `state`, `openIssues`, `changedPaths`.

Read `docs/triage/state.json` if it exists. It carries `computedAt`, the HEAD sha the run
that wrote it was checked out at. Diff against it:

```bash
git diff --name-only <computedAt>..HEAD
```

If this errors — `computedAt` names a commit unreachable from the current checkout, which
happens after a rebase or when the state file came from a different machine — do not
improvise a partial diff and do not skip the code-change axis silently. A `git diff` you
can't run is not evidence that nothing changed under any issue. Instead, skip `invalidate`
entirely and treat every open issue as stale: pass every open issue number as `args.issues`
in step 2, and `args.cached: []`.

Otherwise, feed all three to the invalidation rule — do not reimplement it. There is no form
of doing this safely except writing the JSON to a file and redirecting it in: a heredoc is
out (fish has none), and `echo '<json>' | node ...` breaks the first time an
`impactEvidence` string contains an apostrophe. Write the payload with your file-write tool
(not a shell heredoc), then:

```bash
node scripts/triage/waves.mjs invalidate < /tmp/triage-invalidate-input.json
```

where `/tmp/triage-invalidate-input.json` holds `{"state": <parsed state.json>, "openIssues":
[{"number":.., "digest":..}, ...], "changedPaths": [...]}`.

A cached judgement dies on any of three axes: the issue's content changed, the code under it
moved, or a judgement's evidence names an issue that has closed since the cache was written.
`touches` is an asserted relation between an issue and the code, so the first two axes are
not enough on their own — an issue can sit untouched for months while the file it concerns is
refactored away.

The third axis needs nothing from you and nothing extra in the payload. `invalidate` derives
which issues have closed by subtracting `openIssues` from the cache's own keys: every issue
in `state.json` was open when it was judged, so any of them missing from the open list has
since closed. It then re-judges any cached judgement whose `impactEvidence` or `reproCheck`
mentions one of those numbers as `#N`. This exists because a judgement's severity can hang on
a *third* issue's status — "#287 (still open) is what turns this into an actual bug" — which
neither of the other axes can see coming true (#298). It is a text heuristic on purpose: it
misses a dependency the judge paraphrased instead of numbering, and it fires on a number that
was mentioned in passing. Both cost a re-judgement, never a confident wrong answer that looks
fresh.

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

**This step cannot be delegated to a subagent.** The Workflow tool is only available in the
main session — a subagent's tool set does not include it, the same way it does not include
every tool the main session has. This is a capability boundary, not a rule someone chose to
apply: there is nothing to request permission for, because the tool is simply not present to
grant. If you are a subagent and find no Workflow tool available, stop here and hand this
step back to the main session rather than substitute the Agent tool for it. A pipeline
hand-rolled from Agent calls exercises something other than
`.claude/workflows/backlog-triage.js` — its baseline check, its abort logic, its judgement
schema are all reimplemented ad hoc, or skipped — so a report produced that way says nothing
about whether this capability actually works, while looking exactly like a report that does.

**Before invoking the Workflow tool**, record the PIDs of any matching processes already
running — step 5 needs this to know which ones it started:

```bash
ps -eo pid,command | grep -Ei 'vitest|jest|node .*vite' | grep -v grep | awk '{print $1}' > /tmp/triage-pids-before.txt
```

An empty file here is a normal result — it just means nothing matching was already
running — not a sign of a problem to chase down.

Invoke the Workflow tool with `{scriptPath: ".claude/workflows/backlog-triage.js", args:
{issues: <stale numbers>, cached: <fresh judgement objects, per step 1>}}`.

The script runs the baseline first and aborts if `main` is red on anything that is not
known noise. Its return value carries `aborted` as one of seven distinct strings — never a
boolean — and each means something different:

| `aborted` value | What actually happened | What to say in the report |
|---|---|---|
| `'bad-args'` | The script's own `args` did not resolve to a JSON object — a string that isn't JSON, or a non-object. Nothing was spent: this fires before the baseline agent. | The invocation was malformed, so nothing was triaged. Quote the `log()` line, which echoes what actually arrived. This is a caller bug in step 2, not a finding about the backlog. |
| `'no-issues'` | `args.issues` was empty or missing **and** `args.cached` was empty — nothing to judge and nothing cached to report on. Also fires before the baseline. | Say plainly that the run had no input. This exists so an empty backlog cannot pass for a clean one: a report with zero findings and no abort would look identical to a backlog that was read and found healthy. Check step 1 — most often `fresh` numbers were passed where `cached` judgement objects belong, so both arrays came out empty. |
| `'baseline-agent-died'` | The baseline agent itself returned nothing | The repo's state is **unknown** — the check that would have told you never ran. Do not call this a red baseline. |
| `'suite-red'` | `unknownFailures` was non-empty — a failure not on CLAUDE.md's known-noise list | `main` is red; name the failures |
| `'go-red'` | `go test ./...` did not report green | `go test ./...` is red |
| `'web-red'` | `cd web && npm test` did not report green | the web suite is red |
| `'triage-red'` | `node --test scripts/triage/*.test.mjs` did not report green | the triage suite's own tests are red |

**On the two pre-baseline paths — `'bad-args'` and `'no-issues'` — `baseline` comes back
`null`,** because the abort happens before any baseline agent is spawned. The report's header
format assumes a baseline result, so on these two paths it must instead state that the run
never got as far as checking the tree: there is no baseline to report, red or green, and
writing "baseline unknown" without saying why would read like `'baseline-agent-died'`, which
is a different and much more alarming thing. `'baseline-agent-died'` is the third path with no
`baseline` object — the agent returned nothing, so there is nothing to report there either,
and what the header says on it is that the check never ran, which is still not a red baseline.
The other four abort paths all carry a real `baseline` object.

(The baseline agent runs the suite as three separate legs — `go test ./...`, `node --test
scripts/triage/*.test.mjs`, `cd web && npm test` — because the bare-directory form `node
--test scripts/triage/` fails on this machine's Node; the workflow script already gets this
right, this is only relevant if you are explaining an abort to a human.)

If `aborted` is set at all: report which of the seven happened, in those terms, and stop.
Do not compute waves, do not write the dated report body past the header, do not write
`state.json` — there is nothing new to cache. Ranking issues against a baseline that either
failed or was never checked is ranking fiction either way.

The return value's `unassessed` field is the list of issue numbers whose judge agent
returned nothing at all — the issue was never assessed. Read it directly off the result
(`result.unassessed`), alongside `judgements`, `baseline` and `browser`. It is always an
array, on the success path and on every abort path alike, so there is nothing to test for
before reading it. Do not go looking for it in the workflow's log output — a value the script
already computed belongs in the return value, not in something meant for a human watching the
run.

**Before caching a judgement, stitch on the issue's content digest.** The workflow's
judgement schema has no digest field of its own — it never sees the raw tea issue JSON, only
the collector's distillate — but `waves.mjs invalidate` compares `cached.digest` against each
open issue's freshly computed digest to detect a changed issue. Whichever judgement objects
you write into `docs/triage/state.json` (step 4) must carry the digest you computed for that
issue in step 1, keyed by the same issue number. Skip this and every cached judgement looks
permanently stale (or permanently fresh, if you fabricate a constant) on the next run — the
whole point of caching is gone either way.

### 3. Compute the waves

`result.judgements` (from step 2) already contains every judgement this run knows about —
the freshly judged ones *and* the cached ones passed in as `args.cached`. Pass it to the
wave computation as-is; do not concatenate `cached` onto it again. A duplicate judgement is
not inert: an issue conflicts with itself under the file-overlap rule, so it gets pushed into
a second wave, and the report shows a fake dependency for an issue that has none.

As with `invalidate`, write the payload to a file and redirect it in — never a heredoc, never
an `echo | node` pipe:

```bash
node scripts/triage/waves.mjs waves < /tmp/triage-waves-input.json
```

where `/tmp/triage-waves-input.json` holds `{"issues": <result.judgements, unchanged>,
"contracts": <the "contracts" array from scripts/triage/contracts.json — that file is
`{"contracts": [...]}`, so pull out the array, not the whole file>}`.

**No agent decides what runs in parallel.** Two kinds of edge: file overlap, and shared
contracts. The second exists because file overlap is blind to coupling across a wire — two
issues can change opposite ends of one protocol while touching disjoint files, and that
skew is silent and green, because each side is tested against its own mock.

If a contract is missing from `contracts.json`, the waves cannot see it. Add it there when
you find one.

This also returns two lists of issues it refused to schedule. Both are excluded from every
wave — they never got ranked, they did not rank last — and both go in the report and in
`state.json` (step 4). They are kept apart because they need different repairs:

- `unassessable` — `prodImpact` or `effort` was not one of the recognised values (a typo, an
  omitted field, anything else `isAssessable()` rejects). Fix the judgement's enum value.
- `unschedulable` — a `touches` entry named a directory rather than a file. Fix the
  judgement's `touches` to name the individual files. The judge prompt already forbids
  directories, but a prompt cannot enforce anything: the first real run reported
  `"web/src/api"` on issue 58 anyway. `filesConflict` compares paths for exact equality, so
  that entry never collides with the `web/src/api/stream.tsx` another issue reports — the
  false negative in the dangerous direction, two agents in one file. An issue failing both
  checks is reported as `unassessable` only.

A `touches` entry counts as a directory when it ends in a slash or its last segment has no
dot in it, which is the only signal available to a module that cannot stat the tree. An
extensionless real file (`Makefile`) is therefore excluded too — say so in the report if one
shows up rather than quietly dropping it; it is a one-line fix to the judgement.

### 4. Write the report

Get today's date for the filename rather than guessing it:

```bash
date +%F
```

Write `docs/triage/<that date>-backlog.md`, sections in this order — what demands a human
first:

**Every list of issues below is emitted in an explicitly stated order — never whatever order
the source data happened to have.** A fresh run's `result.judgements` follows the workflow's
fetch order; a regeneration read from `state.json`'s `issues` map instead, and that map's
keys are stringified issue numbers, which JavaScript always enumerates in ascending numeric
order regardless of insertion order, per the language spec. Two different sources, two
different orders, same underlying data — a section that just iterates whichever one it was
handed produces a different file depending on where the run's data came from, and the reports
stop being diffable, which is the entire reason they are dated files on disk instead of a
one-off reply. Each section below says which order it uses; do not add a new list of issues
anywhere in the report without stating one too.

1. **Header** — baseline result (or which of the seven `aborted` values fired), commit,
   judged vs cached counts, browser coverage. On `'bad-args'` and `'no-issues'` there is no
   baseline result at all — `baseline` is `null` — so the header says the run never got as
   far as checking the tree, rather than reporting a baseline it does not have.
2. **Requires your decision** — every issue with a `needsDecision` flag, listed by issue
   number, descending. First, because it is the only part that blocks. A new required
   config key found after a merge stops the container on the next deploy; found here it is a
   line read in ten seconds.
3. **Confirmed still reproducing** — every browser verdict that came back `ISSUES_FOUND`,
   listed by issue number, descending: the defect the issue describes was driven in a real
   browser and the reported behaviour was seen. Give each one the `evidence` string the
   verifier returned. This sits above Waves because it is the only section in the report
   built from *observed* behaviour — everything below it, Waves included, is inferred from
   reading code, and a defect confirmed live outranks any judgement about one. It is also
   the most valuable thing the browser phase can produce, and it is the natural place a
   reader checks a `prodImpact` that now looks too low. Note if the section is empty and
   why: no `ISSUES_FOUND` verdict because every candidate passed is a different fact from no
   `ISSUES_FOUND` verdict because every candidate was `BLOCKED`, and the second says nothing
   about the backlog at all. (The first run hit exactly that case — all four verdicts
   `BLOCKED` — which is why this section was missing from the report layout for as long as
   it was.)
4. **Waves** — bare issue numbers, e.g. `` `#272 #212 #286` ``, kept in the order
   `computeWaves` returned them: highest-ranked issue first within each wave. That order is
   deliberate and carries meaning — do not re-sort it by issue number. Readable by a human
   and pasteable into an implementation run — by a human, or by a separate implementation
   capability. This skill only computes and prints the waves; it never runs one itself. Name,
   right here, any issue whose `touches` came back empty: by design an issue with no known
   file set conflicts with everything, so it alone can push work into later waves. A thin
   wave one with no obvious cause is usually explained by exactly one such issue — say which
   one, because fixing its `touches` (or re-judging it) may collapse several waves into one.
5. **Blocking dependencies** — a mermaid graph of the `statedBlockers` edges only: "this
   issue said it requires that one first," edges listed by the blocked issue's number,
   descending. A real backlog has few of these, so the graph stays small and readable. Do
   not add conflict edges to this graph, here or anywhere else — see the next section for
   why.
6. **Conflict density** — file-overlap and shared-contract conflicts are reported as
   numbers, never a graph: the total conflict-edge count, and the handful of issues driving
   the most of them (e.g. the top three to five by edge count, that list itself ordered by
   descending edge count). A conflict and a dependency are different relations with
   different shapes — "do not work these two at once" is not "do this one first" — and a
   graph mixing both leaves a reader unable to tell which an edge means; a real backlog's
   conflict count is also large enough (hundreds of edges across a few dozen issues is
   normal) that drawing it is both unreadable and beside the point a count and a short
   offender list already make. Keep this section separate from Blocking dependencies for
   that reason, and do not "simplify" the report by merging them back into one graph later.
7. **Full judgement** — a table: issue, kind, impact, evidence, effort, **repro check**,
   touches; rows listed by issue number, descending. This table is a lookup reference, not a
   priority ranking — rank order already lives in Waves, so re-sorting this table by rank
   would just duplicate it under a different name.

   The sixth column holds the judgement's `reproCheck` string verbatim, and its heading is
   **"Repro check"** — not "repro status". The two words mean different things and only one
   of them is in the data: `reproCheck` is the *proposed* falsifiable check ("open this
   route and look at that element"), written before anything was driven, and it is present
   on issues the browser phase never touched. Whether the defect still holds — the
   status — is nowhere in this table. It lives only in the three sections built from browser
   verdicts: Confirmed still reproducing (`ISSUES_FOUND`), Candidates to close (`PASS`), and
   Not verified (`BLOCKED`). Calling this column a status invites a reader to take a
   populated cell as an outcome when it is a plan, so do not rename it back.
8. **Unassessed** — the `unassessed` issue numbers from step 2's result, listed by issue
   number, descending: the judge agent for that issue returned nothing, so it was never
   assessed at all. This is an infrastructure failure worth retrying, not a judgement call —
   say that plainly.
9. **Unassessable** — the `unassessable` issue numbers from step 3, listed by issue number,
   descending: a judgement came back, but its `prodImpact` or `effort` was not a recognised
   value, so the wave computation excluded it. This is a bad judgement worth reading, a
   different failure from Unassessed — keep the two sections separate so a reader can tell
   which happened.
10. **Unschedulable** — the `unschedulable` issue numbers from step 3, listed by issue
    number, descending: the judgement was rankable, but one of its `touches` entries named a
    directory instead of a file, so the wave computation excluded it. Name the offending
    entry for each, because that is the whole repair. A third failure mode, distinct from
    both sections above it: Unassessed has no judgement, Unassessable has one that cannot be
    ranked, this one has a judgement that ranks fine and a file set nothing can be compared
    against.
11. **Candidates to close** — the browser verdicts that came back `PASS`: what was observed,
    listed by issue number, descending. Not a conclusion: "I could not reproduce it" and "it
    is fixed" are different claims, and the second is the maintainer's.
12. **Not verified (BLOCKED)** — listed by issue number, descending: the reason and the
    command that would unblock it. Printing
    that command is the whole job here; running it yourself is not — that is exactly the PR
    lab / browser-verification territory this capability stays out of.

Then write `docs/triage/state.json`. Every value below shown without quotes is a real JSON
object, array or number — not a string; only `computedAt` and the map keys under `issues`
are actual strings:

```json
{
  "computedAt": "<HEAD sha, from git rev-parse HEAD, as a string>",
  "issues": { "<issue number as a string key>": <judgement object, with its digest stitched on> },
  "waves": [[<issue numbers>]],
  "unassessable": [<issue numbers>],
  "unschedulable": [<issue numbers>],
  "unassessed": [<issue numbers>]
}
```

Get `computedAt` with:

```bash
git rev-parse HEAD
```

`issues` holds every judgement this run knows about — reused and newly judged — keyed by
issue number as a string, so next run's `invalidate` step can find them. That is every
*open* issue and nothing else: carrying a closed issue's entry forward would be read next
run as "still open at cache time, closed since", and would permanently stale every judgement
whose evidence names its number. `waves` is what
makes the report checkable after the fact. `unassessable` and `unschedulable` (both from step
3's result) and `unassessed` (from step 2's result) are kept apart deliberately: a judgement
with a value the scheduler didn't recognise, a judgement whose `touches` named a directory,
and no judgement at all. Collapsing any two of them would hide which failure mode actually
happened, and each takes a different repair.

**Leave both files unstaged and uncommitted.** Writing a file and committing it is the
reflex in this repo, but this capability never commits anything — that is the prohibition
at the top of this document, and it applies to its own output just as much as to any
implementation work. Committing them is the maintainer's decision, taken outside this
capability, not a follow-up step of it. The split is deliberate: an agent that commits its
own output has decided, on the human's behalf, that this run's judgement is worth keeping —
and a triage that ran on a bad premise or against a red tree should be discardable without a
revert. This means `git log docs/triage/` is a history of the runs the maintainer chose to
keep, not of every run this capability ever produced — the two are not the same claim, and
this file does not promise the second one.

The first report actually in this repository's history, commit `2933e30`, was committed
deliberately as verification evidence for the plan that built this capability. Finding it in
`git log` is not evidence that committing is this capability's normal behaviour.

### 5. Reap what the run left behind

The script cannot do this — it has no shell. After any run that started a Vite server or a
test runner, list matching processes again and compare against the before-list from step 2 —
by PID set, not by elapsed time, so there is no arithmetic and no `ps` time-format (`etime`
is `[[DD-]HH:]MM:SS`) to convert:

```bash
ps -eo pid,command | grep -Ei 'vitest|jest|node .*vite' | grep -v grep | awk '{print $1}' > /tmp/triage-pids-after.txt
sort /tmp/triage-pids-before.txt -o /tmp/triage-pids-before.txt
sort /tmp/triage-pids-after.txt -o /tmp/triage-pids-after.txt
comm -13 /tmp/triage-pids-before.txt /tmp/triage-pids-after.txt
```

`comm -13` prints only the PIDs unique to the after-list — the ones that did not exist before
this run started. A PID that was already running before the triage started is somebody
else's process, not this run's leftover, and killing it would be worse than leaving an
orphan: the set difference is what keeps this step from touching it. (You can still eyeball
`ps -eo pid,ppid,rss,etime,command` for the same PIDs first — a survivor is recognisable by
having been reparented to `ppid` `1` — but the kill list itself comes from the set
difference, not from reading `ppid` or `etime`.)

Kill every PID `comm` printed:

```bash
kill -9 <pid> <pid> ...
```

Seven orphaned vitest workers have been measured at 4.8 GB.

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
| Trusted `tea issues --output json`'s count as complete | It truncates at a page (30 by default) with no warning; pass an explicit `--limit` and be suspicious of a count landing exactly on a page size |
| Used an issue's `updated` field for cache invalidation | It doesn't exist in `tea`'s output; hash title+body+labels+comments into a digest instead |
| Piped JSON to `waves.mjs` with `echo '...' \| node ...` | Breaks on the first apostrophe in the content; write the payload to a file and redirect it in with `<` |
| Concatenated `cached` onto `result.judgements` before computing waves | `result.judgements` already includes the cached ones; concatenating duplicates them, and a duplicate conflicts with itself, faking a dependency |
| Wrote `state.json`'s template values as quoted strings | Only `computedAt` and the map keys are strings; judgements, numbers and arrays are not — a literal string copy breaks every future cache hit |
| A subagent hand-rolled an Agent-call pipeline when the Workflow tool wasn't available | Stop and hand step 2 back to the main session; a substitute pipeline tests itself, not `.claude/workflows/backlog-triage.js` |
| Drew conflict edges into the dependency graph, or refused to draw anything because the graph was unreadable | They're different relations — graph only `statedBlockers`; report conflict density as a count and top offenders instead |
| Left a thin wave one unexplained | Check for an issue with empty `touches` — it conflicts with everything by design and can push work into later waves on its own; name it in the Waves section |
| Added a new report section that iterates judgements without stating its order | State one — otherwise the section's order silently depends on whether the data came from `result.judgements` or `state.json`, and reading the report won't catch it; only regenerating and diffing will |
| Judged an issue from its text | Read the code it points at; a summary is not a judgement |
| An agent decided the waves | The waves come from `waves.mjs`; agents supply `touches`, not scheduling |
| Ranked issues against a red baseline | The script aborts for a reason — report which `aborted` value fired and stop |
| Treated `aborted: 'baseline-agent-died'` as "the repo is broken" | It means the check never ran, not that it failed — say that, don't conflate the two |
| Reported `'bad-args'` or `'no-issues'` as a baseline problem | Both fire before any baseline agent exists and leave `baseline` `null` — they are caller bugs in step 2 and step 1; say the run never reached the tree |
| Passed `fresh` issue numbers as `args.cached` | The workflow needs judgement *objects*; look each `fresh` number up in `state.json` first |
| Went looking for `unassessed` in the log output | It's a field on the return value (`result.unassessed`), always an array; read it directly |
| Skipped the code-change axis when `computedAt` didn't resolve | Treat everything as stale instead — an unresolvable diff is not evidence nothing changed |
| Kept a closed issue's judgement in `state.json` | The closed-reference axis reads the cache's own keys as "was open then"; a stale entry stales every judgement citing that number, forever |
| Forced a judgement stale by hand because it rested on an issue that had since closed | That axis is mechanical now (#298) — if it didn't fire, the evidence never wrote the number as `#N`; fix the judgement's evidence, not the cache |
| Merged `unassessable`, `unschedulable` and `unassessed` into one bucket | Three different failure modes with three different repairs; keep them apart in the report and in `state.json` |
| Assumed the prompt's "never a directory in `touches`" rule holds | `computeWaves` enforces it now, because the prompt didn't — such an issue lands in `unschedulable` and in no wave; report it and fix the judgement |
| Committed the report or `state.json` | Leave both unstaged; this capability never commits anything, including its own output |
| Called a browser check PASS without rendering | That is BLOCKED; `verifying-ui-in-browser` owns the contract |
| Left an `ISSUES_FOUND` verdict out of the report | It has its own section, above Waves — a defect confirmed live in a browser is the strongest evidence in the report, and `PASS`/`BLOCKED` sections have no room for it |
| Titled the judgement table's sixth column "repro status" | It holds `reproCheck`, a proposed check written before anything was driven — the status lives in the three browser-verdict sections only |
| Two browser verifiers at once | Playwright MCP owns one browser — the loop is serial on purpose |
| Started the lab to unblock a check | Never; report BLOCKED and print `./testenv/lab.sh up` |
| Ran the command printed for a BLOCKED check | Printing it is the job; running it belongs to a separate capability |
| Closed an issue that did not reproduce | It becomes a candidate to close; the decision is the maintainer's |
| Silently verified only the top few | Cap is fine, silence is not — `log()` what was dropped |
| Left a stale entry in the noise list | An entry pointing at a closed issue hides a real failure; flag it |
