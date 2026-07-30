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
     a wrong path puts two agents in the same file. Each entry must name an
     individual FILE, never a directory -- the wave scheduler compares paths for
     exact equality, so a directory never collides with the file paths another
     issue reports and a real conflict would go undetected.

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
