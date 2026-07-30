# Backlog Triage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `/backlog-triage` capability that reads every open Gitea issue, ranks it against production stability, computes which issues can be implemented in parallel, and writes a dated report — without implementing, branching or touching Gitea.

**Architecture:** Three parts with one responsibility each. `scripts/triage/waves.mjs` is pure computation — conflict edges, wave colouring, cache invalidation — and is the only part with unit tests. `.claude/workflows/backlog-triage.js` owns agent fan-out and nothing else. `.claude/skills/backlog-triage/SKILL.md` owns the method and is the entry point.

**Tech Stack:** Node's built-in test runner (`node --test`, no new dependency), the Workflow tool for orchestration, `tea` for Gitea, existing skills `issue-tracker-cli` and `verifying-ui-in-browser`.

## Deviation from the spec

The spec puts wave computation inside the workflow script. Workflow scripts are
self-contained — no imports, no filesystem — so an algorithm living there cannot be unit
tested, while the spec's own verification section demands exactly that ("the waves contain
no pair with overlapping `touches` — mechanical, checkable"; "a second run with no changes
produces a byte-identical report").

So the computation moves to `scripts/triage/waves.mjs`, run via `node` by the skill. The
workflow script keeps the fan-out. Nothing else about the design changes: the computation
is still plain code, still deterministic, still not an agent's judgement.

## Global Constraints

- **No new dependencies.** Tests use `node --test`, which ships with Node.
- **Every `agent()` call names `model` explicitly.** An omitted `model` inherits the session model; a 30-way fan-out without one is 30 Opus agents. Omission is a bug, not a default.
- **Shell independence.** Anything using substitution, a loop, a heredoc, an env prefix or an exit-status variable runs as `bash -c '...'` or from a file via `sh file.sh`. Plain single commands need no wrapper.
- **Never `git add -A`.** Stage explicit paths; agent tooling drops untracked directories in this repo.
- **Commit subjects** follow `<type>: <description>`. This work has no issue number; `chore:` and `docs:` are correct and neither triggers a release.
- **The capability never writes to Gitea, never branches, never commits, never closes an issue, and never starts the PR lab.**

---

### Task 1: Conflict edges from file overlap

**Files:**
- Create: `scripts/triage/waves.mjs`
- Test: `scripts/triage/waves.test.mjs`

**Interfaces:**
- Consumes: nothing.
- Produces: `filesConflict(a, b) -> boolean`, where `a` and `b` are judgement objects with a `touches: string[]` of repo-relative paths.

- [ ] **Step 1: Write the failing test**

```js
// scripts/triage/waves.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { filesConflict } from './waves.mjs'

test('issues touching the same file conflict', () => {
  const a = { number: 1, touches: ['internal/pipeline/importing.go'] }
  const b = { number: 2, touches: ['internal/pipeline/importing.go', 'x.go'] }
  assert.equal(filesConflict(a, b), true)
})

test('issues touching different files in the same directory do not conflict', () => {
  const a = { number: 1, touches: ['internal/pipeline/importing.go'] }
  const b = { number: 2, touches: ['internal/pipeline/discovery.go'] }
  assert.equal(filesConflict(a, b), false)
})

test('an issue with no known touches conflicts with everything', () => {
  const a = { number: 1, touches: [] }
  const b = { number: 2, touches: ['web/src/App.tsx'] }
  assert.equal(filesConflict(a, b), true)
  assert.equal(filesConflict(b, a), true)
})
```

The third case is the spec's error-handling rule: an empty `touches` means we do not know
what the issue changes, so it must not be scheduled beside anything.

- [ ] **Step 2: Run the test to verify it fails**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: FAIL — `Cannot find module` for `./waves.mjs`.

- [ ] **Step 3: Write the minimal implementation**

```js
// scripts/triage/waves.mjs
// Pure computation for backlog triage. No I/O beyond the CLI entry point at the
// bottom, so every rule here is unit-testable -- which is the point: the waves
// decide what agents run in parallel, and a wrong edge costs a merge conflict
// inside a running agent's worktree.

/**
 * True when two issues must not be implemented in the same wave because they
 * change the same file. An issue with no known `touches` conflicts with
 * everything: we cannot prove it is safe, so we do not schedule it beside
 * anything.
 */
export function filesConflict(a, b) {
  if (!a.touches?.length || !b.touches?.length) return true
  const other = new Set(b.touches)
  return a.touches.some(path => other.has(path))
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: PASS, 3 tests.

- [ ] **Step 5: Commit**

```bash
git add scripts/triage/waves.mjs scripts/triage/waves.test.mjs
git commit -m "chore: add file-overlap conflict rule for backlog triage waves"
```

---

### Task 2: Conflict edges from shared contracts

**Files:**
- Create: `scripts/triage/contracts.json`
- Modify: `scripts/triage/waves.mjs`
- Modify: `scripts/triage/waves.test.mjs`

**Interfaces:**
- Consumes: `filesConflict(a, b)` from Task 1.
- Produces: `contractsTouched(issue, contracts) -> string[]` (contract names) and `conflicts(a, b, contracts) -> boolean` combining both edge kinds. `contracts` is an array of `{ name: string, sides: string[][] }` where each side is a list of path prefixes.

- [ ] **Step 1: Write the failing test**

```js
// append to scripts/triage/waves.test.mjs
import { contractsTouched, conflicts } from './waves.mjs'

const CONTRACTS = [
  { name: 'sse-events', sides: [['internal/observ/stream.go'], ['web/src/api/stream.tsx']] },
  { name: 'config', sides: [['internal/config/'], ['config.example.toml']] },
]

test('an issue touching one side of a contract touches the contract', () => {
  const issue = { number: 275, touches: ['internal/observ/stream.go'] }
  assert.deepEqual(contractsTouched(issue, CONTRACTS), ['sse-events'])
})

test('a path prefix matches a directory side', () => {
  const issue = { number: 89, touches: ['internal/config/config.go'] }
  assert.deepEqual(contractsTouched(issue, CONTRACTS), ['config'])
})

test('opposite sides of one contract conflict despite disjoint files', () => {
  const producer = { number: 275, touches: ['internal/observ/stream.go'] }
  const consumer = { number: 267, touches: ['web/src/api/stream.tsx'] }
  assert.equal(filesConflict(producer, consumer), false)
  assert.equal(conflicts(producer, consumer, CONTRACTS), true)
})

test('issues sharing no file and no contract do not conflict', () => {
  const a = { number: 1, touches: ['internal/lidarr/client.go'] }
  const b = { number: 2, touches: ['internal/matcher/rank.go'] }
  assert.equal(conflicts(a, b, CONTRACTS), false)
})
```

The third test is the whole reason this task exists — it is the #275/#267 case, disjoint
files across one protocol.

- [ ] **Step 2: Run the test to verify it fails**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: FAIL — `contractsTouched is not a function`.

- [ ] **Step 3: Write `contracts.json`**

```json
{
  "contracts": [
    {
      "name": "sse-events",
      "why": "GET /api/stream carries a named live frame; producer and consumer must agree on its shape",
      "sides": [["internal/observ/stream.go"], ["web/src/api/stream.tsx"]]
    },
    {
      "name": "album-jobs",
      "why": "the pipeline stages contact each other only through this table",
      "sides": [["internal/store/migrations/"], ["internal/pipeline/"]]
    },
    {
      "name": "config",
      "why": "config loading is strict; a new required key must exist in production before merge",
      "sides": [["internal/config/"], ["config.example.toml"]]
    }
  ]
}
```

- [ ] **Step 4: Write the implementation**

```js
// append to scripts/triage/waves.mjs

/**
 * Names of the shared contracts an issue touches. A side is a list of path
 * prefixes, so both a file and a directory can name a side.
 */
export function contractsTouched(issue, contracts) {
  const touches = issue.touches ?? []
  return contracts
    .filter(c => c.sides.some(side =>
      side.some(prefix => touches.some(path => path.startsWith(prefix)))))
    .map(c => c.name)
}

/**
 * True when two issues must not share a wave. File overlap is the obvious
 * case; the second is not. Two issues can change opposite ends of one protocol
 * while touching entirely disjoint files -- #275 changes the SSE producer,
 * #267 the consumer -- and scheduling those together lets two agents skew a
 * contract silently, because each side is tested against its own mock.
 */
export function conflicts(a, b, contracts) {
  if (filesConflict(a, b)) return true
  const mine = new Set(contractsTouched(a, contracts))
  return contractsTouched(b, contracts).some(name => mine.has(name))
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: PASS, 7 tests.

- [ ] **Step 6: Commit**

```bash
git add scripts/triage/waves.mjs scripts/triage/waves.test.mjs scripts/triage/contracts.json
git commit -m "chore: add shared-contract conflict edges for backlog triage"
```

---

### Task 3: Priority order and wave colouring

**Files:**
- Modify: `scripts/triage/waves.mjs`
- Modify: `scripts/triage/waves.test.mjs`

**Interfaces:**
- Consumes: `conflicts(a, b, contracts)` from Task 2.
- Produces: `rank(issue) -> number` (higher is more urgent) and `computeWaves(issues, contracts) -> number[][]`, an array of waves, each an array of issue numbers in priority order.

- [ ] **Step 1: Write the failing test**

```js
// append to scripts/triage/waves.test.mjs
import { rank, computeWaves } from './waves.mjs'

test('rank orders by prod impact, then by ascending effort', () => {
  const dataloss = { number: 1, prodImpact: 'dataloss', effort: 'L', touches: ['a'] }
  const degraded = { number: 2, prodImpact: 'degraded', effort: 'S', touches: ['b'] }
  assert.ok(rank(dataloss) > rank(degraded))

  const cheap = { number: 3, prodImpact: 'degraded', effort: 'S', touches: ['c'] }
  const dear = { number: 4, prodImpact: 'degraded', effort: 'L', touches: ['d'] }
  assert.ok(rank(cheap) > rank(dear))
})

test('disjoint issues share wave one, ordered by rank', () => {
  const issues = [
    { number: 10, prodImpact: 'cosmetic', effort: 'S', touches: ['a.go'] },
    { number: 11, prodImpact: 'outage', effort: 'M', touches: ['b.go'] },
  ]
  assert.deepEqual(computeWaves(issues, []), [[11, 10]])
})

test('conflicting issues are split across waves, the urgent one first', () => {
  const issues = [
    { number: 20, prodImpact: 'cosmetic', effort: 'S', touches: ['same.go'] },
    { number: 21, prodImpact: 'outage', effort: 'S', touches: ['same.go'] },
  ]
  assert.deepEqual(computeWaves(issues, []), [[21], [20]])
})

test('statedBlockers force ordering even without a file conflict', () => {
  const issues = [
    { number: 30, prodImpact: 'outage', effort: 'S', touches: ['a.go'], statedBlockers: [31] },
    { number: 31, prodImpact: 'cosmetic', effort: 'S', touches: ['b.go'] },
  ]
  assert.deepEqual(computeWaves(issues, []), [[31], [30]])
})

test('computeWaves is deterministic for the same input', () => {
  const issues = [
    { number: 40, prodImpact: 'degraded', effort: 'M', touches: ['a.go'] },
    { number: 41, prodImpact: 'degraded', effort: 'M', touches: ['b.go'] },
    { number: 42, prodImpact: 'degraded', effort: 'M', touches: ['a.go'] },
  ]
  assert.deepEqual(computeWaves(issues, []), computeWaves([...issues].reverse(), []))
})
```

The last test is the spec's byte-identical-report requirement in miniature: if input order
can change the waves, two runs of an unchanged backlog produce different reports and the
whole point of writing them to a file is lost.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: FAIL — `rank is not a function`.

- [ ] **Step 3: Write the implementation**

```js
// append to scripts/triage/waves.mjs

const IMPACT_RANK = { none: 0, cosmetic: 1, degraded: 2, dataloss: 3, outage: 4 }
const EFFORT_COST = { S: 0, M: 1, L: 2 }

/**
 * Priority score. Production impact dominates; cheaper work wins ties, so a
 * wave fills with the most urgent issues that can actually be finished.
 */
export function rank(issue) {
  const impact = IMPACT_RANK[issue.prodImpact] ?? 0
  const effort = EFFORT_COST[issue.effort] ?? 1
  return impact * 10 - effort
}

/**
 * Greedy colouring into waves. Wave one is the largest pairwise-compatible set
 * of the highest-ranked issues; each later wave repeats over what is left.
 *
 * Sorting by (rank desc, number asc) before colouring makes the result
 * independent of input order -- two runs over an unchanged backlog must produce
 * an identical report, or diffing successive reports means nothing.
 */
export function computeWaves(issues, contracts) {
  const ordered = [...issues].sort((a, b) => rank(b) - rank(a) || a.number - b.number)
  const placed = new Map()   // issue number -> wave index
  const waves = []

  for (const issue of ordered) {
    let index = 0
    for (;;) {
      const wave = waves[index] ?? []
      const blocked = (issue.statedBlockers ?? []).some(n => {
        const at = placed.get(n)
        return at === undefined || at >= index
      })
      const clashes = wave.some(other => conflicts(issue, other, contracts))
      if (!blocked && !clashes) break
      index++
    }
    if (!waves[index]) waves[index] = []
    waves[index].push(issue)
    placed.set(issue.number, index)
  }

  return waves.map(wave => wave.map(issue => issue.number))
}
```

A blocker that is not in the backlog at all (`at === undefined`) keeps pushing the issue
to a later wave, so it never lands in wave one on a dependency nobody is working on.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: PASS, 12 tests.

- [ ] **Step 5: Commit**

```bash
git add scripts/triage/waves.mjs scripts/triage/waves.test.mjs
git commit -m "chore: compute triage waves by rank and greedy colouring"
```

---

### Task 4: Two-axis cache invalidation

**Files:**
- Modify: `scripts/triage/waves.mjs`
- Modify: `scripts/triage/waves.test.mjs`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `invalidate({ state, openIssues, changedPaths }) -> { fresh: number[], stale: number[] }`. `state` is `{ computedAt: string, issues: Record<string, judgement & { updated: string }> }`; `openIssues` is `[{ number, updated }]`; `changedPaths` is the output of `git diff --name-only`.

- [ ] **Step 1: Write the failing test**

```js
// append to scripts/triage/waves.test.mjs
import { invalidate } from './waves.mjs'

const state = {
  computedAt: 'f5e1f5b',
  issues: {
    '10': { number: 10, updated: '2026-07-01T00:00:00Z', touches: ['internal/store/jobview.go'] },
    '11': { number: 11, updated: '2026-07-01T00:00:00Z', touches: ['web/src/App.tsx'] },
  },
}

test('an unchanged issue over unchanged code is fresh', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 10, updated: '2026-07-01T00:00:00Z' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [10], stale: [] })
})

test('an issue updated since the cache is stale', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 10, updated: '2026-07-29T00:00:00Z' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [], stale: [10] })
})

test('an issue whose touched code moved is stale even if the issue did not', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 10, updated: '2026-07-01T00:00:00Z' }],
    changedPaths: ['internal/store/jobview.go'],
  })
  assert.deepEqual(out, { fresh: [], stale: [10] })
})

test('an issue absent from the cache is stale', () => {
  const out = invalidate({
    state,
    openIssues: [{ number: 99, updated: '2026-07-29T00:00:00Z' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [], stale: [99] })
})

test('a missing state file makes every open issue stale', () => {
  const out = invalidate({
    state: null,
    openIssues: [{ number: 10, updated: 'x' }, { number: 11, updated: 'y' }],
    changedPaths: [],
  })
  assert.deepEqual(out, { fresh: [], stale: [10, 11] })
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: FAIL — `invalidate is not a function`.

- [ ] **Step 3: Write the implementation**

```js
// append to scripts/triage/waves.mjs

/**
 * Split the open issues into those whose cached judgement still holds and
 * those that must be re-judged.
 *
 * Two independent axes, because `touches` is an asserted relation between an
 * issue and the code: the issue can move, and the code under it can move. An
 * issue may sit untouched for months while the file it concerns is refactored
 * away, so checking only the issue's timestamp is not enough.
 */
export function invalidate({ state, openIssues, changedPaths }) {
  const fresh = []
  const stale = []
  const changed = new Set(changedPaths ?? [])

  for (const open of openIssues) {
    const cached = state?.issues?.[String(open.number)]
    const movedIssue = !cached || cached.updated !== open.updated
    const movedCode = cached
      ? (cached.touches ?? []).some(path =>
          [...changed].some(c => c === path || c.startsWith(path) || path.startsWith(c)))
      : false

    if (movedIssue || movedCode) stale.push(open.number)
    else fresh.push(open.number)
  }

  return { fresh, stale }
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: PASS, 17 tests.

- [ ] **Step 5: Commit**

```bash
git add scripts/triage/waves.mjs scripts/triage/waves.test.mjs
git commit -m "chore: invalidate triage cache on issue and code movement"
```

---

### Task 5: CLI entry point and test wiring

**Files:**
- Modify: `scripts/triage/waves.mjs`
- Modify: `scripts/triage/waves.test.mjs`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `computeWaves` (Task 3) and `invalidate` (Task 4).
- Produces: `node scripts/triage/waves.mjs <mode>` reading JSON on stdin and writing JSON on stdout. Modes: `waves` (input `{issues, contracts}` → `{waves}`) and `invalidate` (input `{state, openIssues, changedPaths}` → `{fresh, stale}`).

- [ ] **Step 1: Write the failing test**

```js
// append to scripts/triage/waves.test.mjs
import { execFileSync } from 'node:child_process'

test('the CLI computes waves from stdin JSON', () => {
  const input = JSON.stringify({
    issues: [
      { number: 1, prodImpact: 'outage', effort: 'S', touches: ['a.go'] },
      { number: 2, prodImpact: 'cosmetic', effort: 'S', touches: ['a.go'] },
    ],
    contracts: [],
  })
  const out = execFileSync('node', ['scripts/triage/waves.mjs', 'waves'], { input })
  assert.deepEqual(JSON.parse(out), { waves: [[1], [2]] })
})

test('the CLI rejects an unknown mode with a non-zero exit', () => {
  assert.throws(() =>
    execFileSync('node', ['scripts/triage/waves.mjs', 'nonsense'], { input: '{}', stdio: 'pipe' }))
})
```

The CLI test runs from the repository root, which is where `make test` runs it.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: FAIL — the CLI produces no output, so `JSON.parse` throws.

- [ ] **Step 3: Write the entry point**

```js
// append to scripts/triage/waves.mjs

// CLI entry point. The skill shells out to this rather than reimplementing the
// rules, so the tested code and the running code are the same code.
if (process.argv[1] && process.argv[1].endsWith('waves.mjs')) {
  const mode = process.argv[2]
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  const input = JSON.parse(chunks.join('') || '{}')

  if (mode === 'waves') {
    process.stdout.write(JSON.stringify({
      waves: computeWaves(input.issues ?? [], input.contracts ?? []),
    }))
  } else if (mode === 'invalidate') {
    process.stdout.write(JSON.stringify(invalidate({
      state: input.state ?? null,
      openIssues: input.openIssues ?? [],
      changedPaths: input.changedPaths ?? [],
    })))
  } else {
    process.stderr.write(`unknown mode: ${mode}\n`)
    process.exit(2)
  }
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `node --test scripts/triage/waves.test.mjs`
Expected: PASS, 19 tests.

- [ ] **Step 5: Wire the tests into `make test`**

Find the `test:` target in `Makefile` and add the triage tests to it, keeping the existing
Go and npm steps intact and in order. The result must run all three:

```make
test:
	go test ./...
	node --test scripts/triage/
	cd web && npm test
```

- [ ] **Step 6: Run the full suite to verify nothing regressed**

Run: `make test`
Expected: Go passes, the triage tests pass, `npm test` reports 362 passed. Note that
`TestOpenRecyclesIdleConnections` (#171) can fail under load — that one is known noise per
CLAUDE.md, and only that one.

- [ ] **Step 7: Commit**

```bash
git add scripts/triage/waves.mjs scripts/triage/waves.test.mjs Makefile
git commit -m "chore: expose triage wave computation as a CLI and run it in make test"
```

---

### Task 6: The workflow script

**Files:**
- Create: `.claude/workflows/backlog-triage.js`

**Interfaces:**
- Consumes: `args` of shape `{ issues: number[], cached: object[] }` supplied by the skill; `scripts/triage/waves.mjs` is *not* called from here (the skill calls it after this script returns).
- Produces: a return value of `{ judgements: judgement[], baseline: object, browser: object[] }` for the skill to feed into the wave computation and the report.

- [ ] **Step 1: Write the script**

```js
// .claude/workflows/backlog-triage.js
export const meta = {
  name: 'backlog-triage',
  description: 'Read every open issue, judge it against production stability, verify what still reproduces',
  phases: [
    { title: 'Collect',  detail: 'distil each issue from tea JSON (haiku)' },
    { title: 'Judge',    detail: 'read the code each issue points at (sonnet)' },
    { title: 'Baseline', detail: 'run the suites once and classify failures' },
    { title: 'Browser',  detail: 'serial browser reproduction of visual claims' },
  ],
}

const JUDGEMENT = {
  type: 'object',
  required: ['number', 'kind', 'prodImpact', 'touches', 'effort'],
  properties: {
    number: { type: 'number' },
    kind: { enum: ['bug', 'feature', 'techdebt', 'test'] },
    prodImpact: { enum: ['none', 'cosmetic', 'degraded', 'dataloss', 'outage'] },
    impactEvidence: { type: 'string' },
    touches: { type: 'array', items: { type: 'string' } },
    frontend: { type: 'boolean' },
    effort: { enum: ['S', 'M', 'L'] },
    reproCheck: { type: ['string', 'null'] },
    concurrency: { type: 'boolean' },
    statedBlockers: { type: 'array', items: { type: 'number' } },
    needsDecision: {
      type: 'object',
      properties: {
        configKey: { type: 'boolean' },
        migration: { type: 'boolean' },
        architecture: { type: 'boolean' },
      },
    },
  },
}

const COLLECTED = {
  type: 'object',
  required: ['number', 'summary', 'paths', 'thin'],
  properties: {
    number: { type: 'number' },
    summary: { type: 'string' },
    paths: { type: 'array', items: { type: 'string' } },
    thin: { type: 'boolean' },
  },
}

const BASELINE = {
  type: 'object',
  required: ['goGreen', 'webGreen', 'unknownFailures'],
  properties: {
    goGreen: { type: 'boolean' },
    webGreen: { type: 'boolean' },
    unknownFailures: { type: 'array', items: { type: 'string' } },
    knownSeen: { type: 'array', items: { type: 'number' } },
  },
}

const VERDICT = {
  type: 'object',
  required: ['number', 'verdict', 'evidence'],
  properties: {
    number: { type: 'number' },
    verdict: { enum: ['PASS', 'ISSUES_FOUND', 'BLOCKED'] },
    evidence: { type: 'string' },
  },
}

const issues = args?.issues ?? []
const cached = args?.cached ?? []

// Baseline first and on its own: ranking thirty issues against a broken
// baseline is ranking fiction, so a failure that is not known noise aborts.
phase('Baseline')
const baseline = await agent(
  `Run the test suites once against the current working tree and classify the result.

   Run each as a single command, naming the interpreter where a construct needs one:
     go test ./...
     cd web && npm test

   Read CLAUDE.md's "Known noise" section first. It lists the failures that are
   known and the conditions under which each is visible. Match every failure you
   see against it by test name.

   Report unknownFailures as the failures that are NOT on that list. Report
   knownSeen as the issue numbers from the list whose failure you actually saw.
   Do not run -race; it is too slow to pay for itself here.`,
  { label: 'baseline', phase: 'Baseline', model: 'sonnet', schema: BASELINE })

if (!baseline || baseline.unknownFailures?.length) {
  log(`main is red: ${baseline?.unknownFailures?.join(', ') ?? 'baseline agent returned nothing'}`)
  return { judgements: [], baseline, browser: [], aborted: true }
}

log(`baseline green (known noise seen: ${baseline.knownSeen?.join(', ') || 'none'})`)

// Collect then judge, pipelined: issue B can be in Judge while issue C is still
// in Collect. Haiku never lets an expensive model see raw tea JSON; sonnet gets
// the distillate and reads the code itself.
const judged = await pipeline(
  issues,
  n => agent(
    `Use the issue-tracker-cli skill for the tea invocation, then read Gitea issue #${n}.

     Extract: a short factual summary of what the issue claims, and every repo
     path the issue text or its comments name. Do not judge severity and do not
     read the code -- that is the next stage's job.

     Set thin: true when the comment thread was long and technical enough that
     your summary may have dropped something load-bearing.`,
    { label: `collect:#${n}`, phase: 'Collect', model: 'haiku', effort: 'low', schema: COLLECTED }),

  (collected, n) => agent(
    `Judge Gitea issue #${n} for a backlog triage. Read CLAUDE.md first.

     A previous agent distilled the issue: ${JSON.stringify(collected)}
     ${collected?.thin ? 'It flagged the thread as thin -- fetch the raw issue JSON yourself.' : ''}

     READ THE CODE the issue points at. Reading only the issue text produces a
     summary, not a judgement, and a summary is worthless here.

     prodImpact is about the running production instance, not about how annoying
     the issue is. Anything above 'cosmetic' needs impactEvidence naming a real
     file:line where it manifests.

     touches must list the repo-relative paths an implementation would change.
     Be accurate: these paths decide which issues are scheduled in parallel, and
     a wrong path puts two agents in the same file.

     reproCheck: a concrete falsifiable check that would decide whether the
     defect still holds -- a route to open, an interaction to drive, a command to
     run. null when there is nothing to reproduce, which is always true of a
     feature request.

     statedBlockers only when the issue text itself says it depends on another.`,
    { label: `judge:#${n}`, phase: 'Judge', model: 'sonnet', schema: JUDGEMENT }))

// A dead agent must not vanish. Dropping the issue silently would make the
// report read as complete coverage of a backlog it never finished reading.
const unassessed = issues.filter((n, i) => !judged[i])
if (unassessed.length) log(`unassessed (agent returned nothing): ${unassessed.map(n => `#${n}`).join(' ')}`)

const judgements = [...cached, ...judged.filter(Boolean)]
log(`${judged.filter(Boolean).length} judged, ${cached.length} reused from cache`)

// Browser reproduction is serial: the Playwright MCP server owns one browser for
// the session, and two verifiers at once return verdicts about each other's tab.
phase('Browser')
const candidates = judgements
  .filter(j => j.frontend && j.reproCheck && j.kind !== 'feature')
  .sort((a, b) => (b.prodImpact === 'outage') - (a.prodImpact === 'outage'))

const BROWSER_CAP = 4
const selected = candidates.slice(0, BROWSER_CAP)
if (candidates.length > BROWSER_CAP) {
  log(`browser cap: verifying ${BROWSER_CAP} of ${candidates.length}; skipped ${candidates.slice(BROWSER_CAP).map(c => `#${c.number}`).join(' ')}`)
}

const browser = []
for (const item of selected) {
  browser.push(await agent(
    `Invoke the verifying-ui-in-browser skill and follow it exactly, for Gitea issue #${item.number}.

     Nothing has been implemented -- you are checking whether the defect the
     issue describes still reproduces on the current code. The check to drive:
     ${item.reproCheck}

     The lab backend may already be running on http://localhost:9090. Probe it
     with: curl -sf http://localhost:9090/status
     If it does not answer, your verdict is BLOCKED. Do NOT start the lab: it
     logs into a real Soulseek account, takes minutes, and only one can run.

     Serve the frontend from this checkout on a free port with
     SLSKDARR_DEV_API=http://localhost:9090, using env to set it so the command
     works regardless of shell.

     Return the three-value verdict with evidence. Reading the CSS instead of
     rendering it is BLOCKED, never PASS.`,
    { label: `browser:#${item.number}`, phase: 'Browser', model: 'sonnet', schema: VERDICT }))
}

return { judgements, baseline, browser: browser.filter(Boolean) }
```

- [ ] **Step 2: Verify the script parses**

The Workflow tool rejects a script that does not parse, and a syntax error costs a whole
run to discover. Check it directly first:

```bash
node --input-type=module --check < .claude/workflows/backlog-triage.js
```

Expected: no output, exit 0.

- [ ] **Step 3: Verify every agent call names a model**

```bash
grep -c 'agent(' .claude/workflows/backlog-triage.js
grep -c "model: '" .claude/workflows/backlog-triage.js
```

Expected: both print `4`. If they differ, an `agent()` call is missing its `model` and
would silently inherit the session model.

- [ ] **Step 4: Commit**

```bash
git add .claude/workflows/backlog-triage.js
git commit -m "chore: add the backlog-triage workflow script"
```

---

### Task 7: The skill

**Files:**
- Create: `.claude/skills/backlog-triage/SKILL.md`

**Interfaces:**
- Consumes: `.claude/workflows/backlog-triage.js` (Task 6) and `node scripts/triage/waves.mjs` (Task 5).
- Produces: `/backlog-triage`, writing `docs/triage/YYYY-MM-DD-backlog.md` and `docs/triage/state.json`.

- [ ] **Step 1: Write the skill**

```markdown
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

### 2. Run the workflow

Invoke the Workflow tool with `{scriptPath: ".claude/workflows/backlog-triage.js", args: {issues: <stale>, cached: <fresh judgements from state.json>}}`.

The script runs the baseline first and aborts if `main` is red on anything that is not
known noise. Honour that: report the single finding and stop. Ranking thirty issues against
a broken baseline is ranking fiction.

### 3. Compute the waves

```bash
node scripts/triage/waves.mjs waves
```

with `{"issues": <all judgements>, "contracts": <contracts array from scripts/triage/contracts.json>}`.

**No agent decides what runs in parallel.** Two kinds of edge: file overlap, and shared
contracts. The second exists because file overlap is blind to coupling across a wire — two
issues can change opposite ends of one protocol while touching disjoint files, and that
skew is silent and green, because each side is tested against its own mock.

If a contract is missing from `contracts.json`, the waves cannot see it. Add it there when
you find one.

### 4. Write the report

`docs/triage/YYYY-MM-DD-backlog.md`, sections in this order — what demands a human first:

1. **Header** — baseline result, commit, judged vs cached counts, browser coverage.
2. **Requires your decision** — every issue with a `needsDecision` flag. First, because it
   is the only part that blocks. A new required config key found after a merge stops the
   container on the next deploy; found here it is a line read in ten seconds.
3. **Waves** — bare issue numbers, e.g. `` `#272 #212 #286` ``. Readable by a human and
   pasteable into an implementation run.
4. **Dependencies** — a mermaid graph of the conflict and blocker edges.
5. **Full judgement** — a table: issue, impact, evidence, effort, repro status, touches.
6. **Candidates to close** — what was observed. Not a conclusion: "I could not reproduce
   it" and "it is fixed" are different claims, and the second is the maintainer's.
7. **Not verified (BLOCKED)** — with the reason and the command that would unblock it.

Then write `docs/triage/state.json` as `{"computedAt": "<HEAD sha>", "issues": {"<number>":
<judgement with its `updated` timestamp>}, "waves": [[<numbers>]], "unassessed":
[<numbers>]}`. The `waves` array is what makes the report checkable after the fact, and
`unassessed` records any issue whose agent returned nothing — a silent drop would let the
report read as full coverage of a backlog it never finished.

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
| Ranked issues against a red baseline | The script aborts for a reason — report the failure and stop |
| Called a browser check PASS without rendering | That is BLOCKED; `verifying-ui-in-browser` owns the contract |
| Two browser verifiers at once | Playwright MCP owns one browser — the loop is serial on purpose |
| Started the lab to unblock a check | Never; report BLOCKED and print `./testenv/lab.sh up` |
| Closed an issue that did not reproduce | It becomes a candidate to close; the decision is the maintainer's |
| Silently verified only the top few | Cap is fine, silence is not — `log()` what was dropped |
| Left a stale entry in the noise list | An entry pointing at a closed issue hides a real failure; flag it |
```

- [ ] **Step 2: Verify the skill is discoverable**

```bash
ls .claude/skills/backlog-triage/SKILL.md
grep -c '^name: backlog-triage' .claude/skills/backlog-triage/SKILL.md
```

Expected: the path exists and the grep prints `1`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/backlog-triage/SKILL.md
git commit -m "chore: add the backlog-triage skill"
```

---

### Task 8: Dry run against the real backlog

**Files:**
- Create: `docs/triage/2026-07-30-backlog.md` (generated)
- Create: `docs/triage/state.json` (generated)

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces: the first real report, and evidence for the spec's four verification checks.

There is nothing to unit test here: the deliverable is a judgement. Verification is a real
run, checked on four points.

- [ ] **Step 1: Run it**

Invoke `/backlog-triage` and let it complete.

- [ ] **Step 2: Check the waves contain no conflicting pair**

Mechanical, from the generated state:

```bash
node --input-type=module -e "
import {readFileSync} from 'node:fs'
import {conflicts} from './scripts/triage/waves.mjs'
const s = JSON.parse(readFileSync('docs/triage/state.json','utf8'))
const c = JSON.parse(readFileSync('scripts/triage/contracts.json','utf8')).contracts
const by = s.issues
let bad = 0
for (const wave of s.waves) for (const a of wave) for (const b of wave) {
  if (a < b && conflicts(by[a], by[b], c)) { console.log('CONFLICT', a, b); bad++ }
}
console.log(bad === 0 ? 'OK: no conflicting pair shares a wave' : 'FAILED')
"
```

Expected: `OK`.

- [ ] **Step 3: Check every impact claim carries evidence**

Read the report's full-judgement table. Every issue with `prodImpact` above `cosmetic` must
have an `impactEvidence` naming a `file:line`. Open two of them and confirm the line says
what the evidence claims. If any is fabricated, that is a finding about the judge prompt,
not about the issue.

- [ ] **Step 4: Check a second run is byte-identical**

```bash
cp docs/triage/2026-07-30-backlog.md /tmp/triage-first.md
```

Run `/backlog-triage` again without changing anything, then:

```bash
diff /tmp/triage-first.md docs/triage/2026-07-30-backlog.md && echo "IDENTICAL"
```

Expected: `IDENTICAL`. This is the check that matters most — a triage whose output drifts
between identical runs cannot be diffed, and being diffable is most of the value. If it
differs, the cause is either the cache not being reused or the synthesis agent rewriting
prose it should have copied.

- [ ] **Step 5: Check invalidation is precise**

Touch a file named in one cached issue's `touches`, then re-run and confirm exactly that
issue was re-judged and no other:

```bash
git diff --name-only HEAD
```

Compare against the report header's judged-vs-cached counts.

- [ ] **Step 6: Reap orphaned workers**

```bash
ps -eo pid,ppid,rss,etime,command | grep -Ei 'vitest|jest|node .*vite' | grep -v grep
```

Kill the `ppid=1` rows from this run.

- [ ] **Step 7: Commit the first report**

```bash
git add docs/triage/2026-07-30-backlog.md docs/triage/state.json
git commit -m "docs: first backlog triage report"
```

---

## Open questions for Samuel

- **No issue number.** This work has no Gitea issue. The repo convention is a branch per
  issue; `chore/` branches are exempt, so nothing is wrong, but if you want it tracked,
  create the issue before Task 8 so the report can reference it.
- **`docs/triage/` is new.** Nothing gitignores it and nothing else writes there. If you
  would rather the reports live under `docs/superpowers/` alongside the specs, that is a
  one-line change in Task 7 step 1 and Task 8.
