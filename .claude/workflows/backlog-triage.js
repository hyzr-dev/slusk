// .claude/workflows/backlog-triage.js
export const meta = {
  name: 'backlog-triage',
  description: 'Read every open issue, judge it against production stability, verify what still reproduces',
  phases: [
    { title: 'Baseline', detail: 'run the suites once and classify failures' },
    { title: 'Collect',  detail: 'distil each issue from tea JSON (haiku)' },
    { title: 'Judge',    detail: 'read the code each issue points at (sonnet)' },
    { title: 'Browser',  detail: 'serial browser reproduction of visual claims' },
  ],
}

const JUDGEMENT = {
  type: 'object',
  required: ['number', 'kind', 'prodImpact', 'impactEvidence', 'touches', 'frontend', 'effort', 'reproCheck', 'statedBlockers'],
  properties: {
    number: { type: 'number' },
    kind: { enum: ['bug', 'feature', 'techdebt', 'test'] },
    prodImpact: { enum: ['none', 'cosmetic', 'degraded', 'dataloss', 'outage'] },
    // minLength only rules out the empty string -- the original hole. It
    // deliberately does not try to express the real rule ("a path:line
    // reference is required when prodImpact is above cosmetic"), because a
    // length floor can't state a conditional-on-content rule; that lives in
    // the judge prompt below instead, where a reviewer reading the report can
    // actually check it against the field's content.
    impactEvidence: { type: 'string', minLength: 1 },
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
  required: ['goGreen', 'webGreen', 'triageGreen', 'unknownFailures', 'treeStatus'],
  properties: {
    goGreen: { type: 'boolean' },
    webGreen: { type: 'boolean' },
    triageGreen: { type: 'boolean' },
    unknownFailures: { type: 'array', items: { type: 'string' } },
    knownSeen: { type: 'array', items: { type: 'number' } },
    treeStatus: { type: 'string' },
  },
}

// Mirrors the rank() ordering in scripts/triage/waves.mjs -- a Workflow script
// cannot import that module, so these tables are duplicated here and must be
// kept in step with it by hand.
const IMPACT_RANK = { none: 0, cosmetic: 1, degraded: 2, dataloss: 3, outage: 4 }
const EFFORT_COST = { S: 0, M: 1, L: 2 }

const VERDICT = {
  type: 'object',
  required: ['number', 'verdict', 'evidence'],
  properties: {
    number: { type: 'number' },
    verdict: { enum: ['PASS', 'ISSUES_FOUND', 'BLOCKED'] },
    evidence: { type: 'string' },
  },
}

// Validate args before spending anything on a baseline agent: a malformed
// invocation should cost nothing, not burn an agent before the emptiness is
// discovered.
//
// The Workflow tool's own docs say to pass args as an actual JSON value, not a
// stringified payload -- and yet a stringified payload is what arrives here in
// practice. So this accepts either shape rather than being correct-but-unusable:
// a future reader who deletes this branch as "redundant, the docs already say
// not to do this" will break every real invocation, and the failure will look
// exactly like an empty backlog rather than a rejected input.
let normalizedArgs = args
if (typeof args === 'string') {
  try {
    normalizedArgs = JSON.parse(args)
  } catch {
    log(`args arrived as a string that isn't valid JSON -- cannot recover a payload from it. Received: ${args}`)
    return { judgements: [], baseline: null, browser: [], unassessed: [], aborted: 'bad-args' }
  }
}
if (typeof normalizedArgs !== 'object' || normalizedArgs === null || Array.isArray(normalizedArgs)) {
  log(`args must resolve to a JSON object (either passed as one, or as a JSON string of one), not
     ${typeof normalizedArgs}. Expected shape: { issues: number[], cached: object[] }. Received: ${JSON.stringify(args)}`)
  return { judgements: [], baseline: null, browser: [], unassessed: [], aborted: 'bad-args' }
}
args = normalizedArgs

const cached = args?.cached ?? []

// args.issues missing, not an array, or empty is fine BY ITSELF -- an empty
// issues array with a populated cache is the normal second-run case (every
// issue cached, nothing stale) and must NOT abort. It's only meaningless when
// there is ALSO nothing cached: no fresh issue to judge and nothing to report
// on either.
const issuesArr = Array.isArray(args?.issues) ? args.issues : null
if ((!issuesArr || issuesArr.length === 0) && cached.length === 0) {
  log(`no issues to triage: args.issues is ${issuesArr ? `empty` : `missing or not an array (${typeof args?.issues})`} and args.cached is empty -- nothing to judge and nothing cached to report on`)
  return { judgements: [], baseline: null, browser: [], unassessed: [], aborted: 'no-issues' }
}
const issues = issuesArr ?? []

// Baseline first and on its own: ranking thirty issues against a broken
// baseline is ranking fiction, so a failure that is not known noise aborts.
phase('Baseline')
const baseline = await agent(
  `Run the test suites once against the current working tree and classify the result.
   This is a read-only check: do not edit, stash, or commit anything, even if a
   failure looks trivial to fix -- roughly thirty other agents are about to read
   this same tree and every one of them needs to see what you saw, not a tree
   you patched.

   Run each as a single command, naming the interpreter where a construct needs one:
     go test ./...
     node --test scripts/triage/*.test.mjs
     cd web && npm test

   Read CLAUDE.md's "Known noise" section first. It lists the failures that are
   known and the conditions under which each is visible. Match every failure you
   see against it by test name.

   Report unknownFailures as the failures that are NOT on that list. Report
   knownSeen as the issue numbers from the list whose failure you actually saw.
   Do not run -race; it is too slow to pay for itself here.

   Finally run \`git status --short\` and report its raw output as treeStatus --
   this is the evidence that you left the tree exactly as you found it.`,
  { label: 'baseline', phase: 'Baseline', model: 'sonnet', schema: BASELINE })

if (!baseline) {
  log('baseline agent returned nothing -- treating as aborted, not as a verdict on the repository')
  return { judgements: [], baseline, browser: [], unassessed: [], aborted: 'baseline-agent-died' }
}
if (baseline.unknownFailures?.length) {
  log(`main is red: ${baseline.unknownFailures.join(', ')}`)
  return { judgements: [], baseline, browser: [], unassessed: [], aborted: 'suite-red' }
}
// The three greens are required fields in their own right: an agent can report
// empty unknownFailures alongside a false green (e.g. it classified everything
// as known noise, or filled the boolean and not the list), which would
// otherwise slip through as a clean baseline. Ranking issues against that is
// ranking fiction, same as an outright unknownFailures hit.
if (!baseline.goGreen) {
  log('main is red: go test ./... did not report green')
  return { judgements: [], baseline, browser: [], unassessed: [], aborted: 'go-red' }
}
if (!baseline.webGreen) {
  log('main is red: npm test (web/) did not report green')
  return { judgements: [], baseline, browser: [], unassessed: [], aborted: 'web-red' }
}
if (!baseline.triageGreen) {
  log('main is red: node --test scripts/triage did not report green')
  return { judgements: [], baseline, browser: [], unassessed: [], aborted: 'triage-red' }
}

log(`baseline green (known noise seen: ${baseline.knownSeen?.join(', ') || 'none'})`)

// Collect then judge, pipelined: issue B can be in Judge while issue C is still
// in Collect. Haiku never lets an expensive model see raw tea JSON; sonnet gets
// the distillate and reads the code itself.
const judged = await pipeline(
  issues,
  n => agent(
    `Use the issue-tracker-cli skill for the tea invocation, then read Gitea issue #${n}
     with --comments -- without that flag the comments never come back at all.

     Extract: a short factual summary of what the issue claims, and every repo
     path the issue text or its comments name. Do not judge severity and do not
     read the code -- that is the next stage's job.

     If the issue no longer exists or is already closed, still return all four
     required fields: say so plainly in summary, and use an empty paths array.

     Set thin: true when the comment thread was long and technical enough that
     your summary may have dropped something load-bearing.`,
    { label: `collect:#${n}`, phase: 'Collect', model: 'haiku', effort: 'low', schema: COLLECTED }),

  (collected, n) => agent(
    `Judge Gitea issue #${n} for a backlog triage. Read CLAUDE.md first.

     ${collected
       ? `A previous agent distilled the issue: ${JSON.stringify(collected)}`
       : 'The previous agent that was supposed to distil this issue returned nothing -- fetch the raw issue JSON yourself.'}
     ${collected?.thin ? 'It flagged the thread as thin -- fetch the raw issue JSON yourself.' : ''}

     READ THE CODE the issue points at. Reading only the issue text produces a
     summary, not a judgement, and a summary is worthless here.

     prodImpact is about the running production instance, not about how annoying
     the issue is. impactEvidence is a required field on every issue, not just the
     severe ones. When prodImpact is above 'cosmetic', impactEvidence must be a
     path:line reference a reader can open -- not a description, not a
     restatement of the issue title. When it's 'none' or 'cosmetic', say briefly
     why there is no production impact instead of leaving it empty. A reviewer
     will open the reference you give against the actual file: one that doesn't
     resolve, or doesn't say what you claimed, is worse than an honest lower
     prodImpact. If a range is cited, it must contain every element of the
     claim -- name two separate references rather than one range that spans
     them but only actually contains one, since a range that opens to real code
     but omits part of what it claims reads as verified when it is not.

     touches must list the repo-relative paths an implementation would change.
     Be accurate: these paths decide which issues are scheduled in parallel, and
     a wrong path puts two agents in the same file. Each entry must name an
     individual FILE, never a directory -- the wave scheduler compares paths for
     exact equality, so a directory never collides with the file paths another
     issue reports and a real conflict would go undetected.

     frontend: true only when the issue is about the React SPA in web/ or its
     rendered behaviour -- this gates whether the issue is even considered for
     browser verification, so leaving it unset is read as false, not unknown.

     concurrency: true when the issue involves goroutines, channels, locking, or
     race conditions -- this flags issues that need -race attention beyond what
     the baseline run covers.

     reproCheck: a concrete falsifiable check that would decide whether the
     defect still holds -- a route to open, an interaction to drive, a command to
     run. null when there is nothing to reproduce, which is always true of a
     feature request.

     statedBlockers only when the issue text itself says it depends on another;
     report an empty array, not an omitted field, when there are none.`,
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
  .filter(j => j?.frontend && j?.reproCheck && j?.kind !== 'feature')
  .sort((a, b) => (IMPACT_RANK[b.prodImpact] - IMPACT_RANK[a.prodImpact])
    || (EFFORT_COST[a.effort] - EFFORT_COST[b.effort])
    || (a.number - b.number))

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
     issue describes still reproduces on the current code. That inverts what the
     skill's own verdict names normally mean, since the skill was written to
     verify a fix, not to reproduce a defect. Use this mapping instead:
       - The defect does NOT reproduce (behaviour looks correct)  -> PASS
       - The defect DOES reproduce (you saw the reported problem) -> ISSUES_FOUND
       - You could not render or reach the app at all              -> BLOCKED
     Seeing the exact broken behaviour the issue describes is ISSUES_FOUND, not
     PASS -- PASS here means the bug is gone, not that you successfully observed it.

     The check to drive:
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

return { judgements, baseline, browser: browser.filter(Boolean), unassessed }
