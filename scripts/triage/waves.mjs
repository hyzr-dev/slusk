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

/**
 * Names of the shared contracts an issue touches. A side is a list of path
 * prefixes, so both a file and a directory can name a side.
 */
export function contractsTouched(issue, contracts) {
  const touches = issue.touches ?? []
  return contracts
    .filter(c => c.sides.some(side =>
      side.some(prefix => touches.some(path => {
        // Remove trailing slash from prefix for consistent matching
        const normalized = prefix.endsWith('/') ? prefix.slice(0, -1) : prefix
        // Match exactly (file) or with slash following (directory)
        return path === normalized || path.startsWith(normalized + '/')
      }))))
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

const IMPACT_RANK = { none: 0, cosmetic: 1, degraded: 2, dataloss: 3, outage: 4 }
const EFFORT_COST = { S: 0, M: 1, L: 2 }

/**
 * True when an issue can be ranked. Returns false if prodImpact or effort are
 * not recognized — a typo'd `"outage"` would rank as `none` silently, which is
 * the opposite of what a production-impact ranking exists for.
 */
export function isAssessable(issue) {
  return issue.prodImpact in IMPACT_RANK && issue.effort in EFFORT_COST
}

/**
 * Priority score. Production impact dominates; cheaper work wins ties, so a
 * wave fills with the most urgent issues that can actually be finished.
 * Defaults to `none` and `M` respectively so ranking stays total.
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
 *
 * Issues with stated blockers that are in the backlog are sorted after
 * unblocked issues — a real departure from the maximal wave-one goal, but
 * necessary: processing blockers first is what makes the placement loop
 * terminate when blockers have lower rank than blocked issues.
 *
 * Cyclic blockers (A blocks B, B blocks A) do not hang; one is silently placed
 * before its own blocker. Cyclic input is malformed and resolved arbitrarily.
 */
export function computeWaves(issues, contracts) {
  const issueNumbers = new Set(issues.map(i => i.number))
  const assessable = issues.filter(isAssessable)
  const unassessable = issues.filter(i => !isAssessable(i)).map(i => i.number)

  const ordered = [...assessable].sort((a, b) => {
    const aHasBlockers = (a.statedBlockers ?? []).some(n => issueNumbers.has(n))
    const bHasBlockers = (b.statedBlockers ?? []).some(n => issueNumbers.has(n))
    if (aHasBlockers !== bHasBlockers) return aHasBlockers - bHasBlockers
    return rank(b) - rank(a) || a.number - b.number
  })
  const placed = new Map()   // issue number -> wave index
  const waves = []

  for (const issue of ordered) {
    let index = 0
    for (;;) {
      const wave = waves[index] ?? []
      const blocked = (issue.statedBlockers ?? []).some(n => {
        if (!issueNumbers.has(n)) return false
        const at = placed.get(n)
        return at !== undefined && at >= index
      })
      const clashes = wave.some(other => conflicts(issue, other, contracts))
      if (!blocked && !clashes) break
      index++
    }
    if (!waves[index]) waves[index] = []
    waves[index].push(issue)
    placed.set(issue.number, index)
  }

  return {
    waves: waves.map(wave => wave.map(issue => issue.number)),
    unassessable
  }
}

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

// CLI entry point. The skill shells out to this rather than reimplementing the
// rules, so the tested code and the running code are the same code.
if (process.argv[1] && process.argv[1].endsWith('waves.mjs')) {
  const mode = process.argv[2]
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  const input = JSON.parse(chunks.join('') || '{}')

  if (mode === 'waves') {
    process.stdout.write(JSON.stringify(computeWaves(input.issues ?? [], input.contracts ?? [])))
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
