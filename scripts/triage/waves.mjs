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
 * True when a `touches` entry names a directory rather than an individual file.
 *
 * Recognised two ways, because this module is pure computation and cannot stat
 * anything: a trailing slash, or a final path segment with no dot in it.
 *
 * `internal/pipeline/` and `web/src/api` both come back true; extensionless
 * files that really exist (`Makefile`, `Dockerfile`) come back true as well.
 * That false positive is the safe direction -- it excludes an issue from the
 * waves and says so out loud, where the failure it guards against is silent --
 * and a reader who sees `Makefile` in the excluded list can fix the judgement in
 * one line.
 *
 * What it misses, symmetrically: a directory whose last segment contains a dot
 * (`docs/v1.2`, a bare `.claude`) reads as a file and slips through. Not worth
 * chasing -- a path-shaped string cannot be classified perfectly without
 * touching the filesystem, and this module is deliberately pure -- but the guard
 * is a heuristic, not a decision procedure, and a caller should not read it as
 * complete.
 */
export function namesDirectory(path) {
  const trimmed = path.trim()
  if (trimmed.endsWith('/')) return true
  return !trimmed.slice(trimmed.lastIndexOf('/') + 1).includes('.')
}

/**
 * True when any of an issue's `touches` entries names a directory.
 *
 * A directory entry is unusable, not merely untidy. `filesConflict` compares
 * paths for exact equality, so `"web/src/api"` never collides with the
 * `web/src/api/stream.tsx` or `web/src/api/queries.ts` another issue reports:
 * the entry looks like it declares a file set and in fact declares nothing the
 * scheduler can see. The result is a false negative in the dangerous direction
 * -- two agents scheduled into one file, which is the exact cost the waves exist
 * to avoid -- and unlike an empty `touches`, which conflicts with everything and
 * so fails safe, this one fails silent. The first real run produced such an
 * entry (issue 58, `"web/src/api"`) while the judge prompt already forbade it,
 * which is why the check belongs here and not in the prompt.
 */
export function hasDirectoryTouch(issue) {
  return (issue.touches ?? []).some(namesDirectory)
}

/**
 * The (contract, side) pairs an issue touches. A side is a list of path
 * prefixes, so both a file and a directory can name a side; its index within
 * the contract's `sides` array is a sufficient identity for it.
 *
 * An issue can appear against more than one side of the same contract -- an
 * issue that changes both the SSE producer and its consumer, say. That is
 * exactly the case `conflicts` must still treat as a clash against anything
 * else on that contract: touching every side is strictly more entangled with
 * the protocol than touching one, never less, so it must not be read as
 * cancelling out into "no side" or "safe".
 */
export function contractsTouched(issue, contracts) {
  const touches = issue.touches ?? []
  const result = []
  for (const c of contracts) {
    c.sides.forEach((side, sideIndex) => {
      const matches = side.some(prefix => {
        // Remove trailing slash from prefix for consistent matching
        const normalized = prefix.endsWith('/') ? prefix.slice(0, -1) : prefix
        // Match exactly (file) or with slash following (directory)
        return touches.some(path => path === normalized || path.startsWith(normalized + '/'))
      })
      if (matches) result.push({ name: c.name, side: sideIndex })
    })
  }
  return result
}

/**
 * True when two issues must not share a wave. File overlap is the obvious
 * case; the second is not. Two issues can change opposite ends of one protocol
 * while touching entirely disjoint files -- #275 changes the SSE producer,
 * #267 the consumer -- and scheduling those together lets two agents skew a
 * contract silently, because each side is tested against its own mock.
 *
 * The comparison is by (contract, side), not by contract name alone: two
 * issues that both only touch the producer side, say, are both working on the
 * same end of the same protocol and are exactly the kind of change that
 * benefits from being in the same wave, not split apart by a rule meant for
 * the opposite case.
 */
export function conflicts(a, b, contracts) {
  if (filesConflict(a, b)) return true
  const mine = contractsTouched(a, contracts)
  const theirs = contractsTouched(b, contracts)
  return mine.some(x => theirs.some(y => x.name === y.name && x.side !== y.side))
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
 * True when an issue is a sound candidate for browser-based reproduction.
 * Three conditions, each excluding a case a browser session cannot settle:
 * `frontend` and `reproCheck` are the obvious prerequisites -- there must be
 * something in `web/` to load and a concrete check to run against it -- and
 * `kind` rules out two further cases. A `feature` has nothing shipped yet to
 * reproduce against: there is no existing behaviour for a browser session to
 * compare against a description. A `test` issue's defect lives in a test
 * command's output, not in the running application, so no browser session can
 * falsify or confirm it either way.
 */
export function isBrowserVerifiable(issue) {
  return Boolean(issue.frontend) && Boolean(issue.reproCheck) &&
    issue.kind !== 'feature' && issue.kind !== 'test'
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
 *
 * Two exclusions are returned rather than scheduled, and they are kept apart
 * because they need different repairs: `unassessable` means the judgement's
 * `prodImpact` or `effort` was not a recognised value, `unschedulable` means its
 * `touches` named a directory (see hasDirectoryTouch) and so cannot be compared
 * against anything. An issue failing both is reported as `unassessable` only --
 * there is no ranking to schedule either way, and listing it twice would
 * double-count it in the report.
 */
export function computeWaves(issues, contracts) {
  const issueNumbers = new Set(issues.map(i => i.number))
  const unassessable = issues.filter(i => !isAssessable(i)).map(i => i.number)
  const unschedulable = issues
    .filter(i => isAssessable(i) && hasDirectoryTouch(i))
    .map(i => i.number)
  const assessable = issues.filter(i => isAssessable(i) && !hasDirectoryTouch(i))

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
    unassessable,
    unschedulable
  }
}

/**
 * Split the open issues into those whose cached judgement still holds and
 * those that must be re-judged.
 *
 * Two independent axes, because `touches` is an asserted relation between an
 * issue and the code: the issue can move, and the code under it can move. An
 * issue may sit untouched for months while the file it concerns is refactored
 * away, so checking only the issue's content would miss the refactoring.
 *
 * The digest is a content hash computed by the caller and is expected to cover
 * the issue's title, body, labels, and comments — enough to detect real content
 * change and ignore touches that changed nothing.
 */
export function invalidate({ state, openIssues, changedPaths }) {
  const fresh = []
  const stale = []
  const changed = new Set(changedPaths ?? [])

  for (const open of openIssues) {
    const cached = state?.issues?.[String(open.number)]
    const movedIssue = !cached || cached.digest !== open.digest
    const movedCode = cached
      ? (cached.touches ?? []).some(path =>
          [...changed].some(c => c === path || c.startsWith(path) || path.startsWith(c)))
      : false

    if (movedIssue || movedCode) stale.push(open.number)
    else fresh.push(open.number)
  }

  return { fresh, stale }
}

/**
 * Reassemble a stdin byte stream captured as an array of chunks into text.
 * Buffers must be concatenated before decoding -- decoding each chunk to a
 * string independently and joining the strings corrupts any multi-byte
 * character that a chunk boundary happens to split in half, which for this
 * repo's Swedish issue titles (å, ä, ö) is not a corner case: it is routine
 * as soon as the backlog is large enough for stdin to arrive in more than
 * one chunk.
 */
export function decodeChunks(chunks) {
  return Buffer.concat(chunks).toString('utf8')
}

// CLI entry point. The skill shells out to this rather than reimplementing the
// rules, so the tested code and the running code are the same code.
if (process.argv[1] && process.argv[1].endsWith('waves.mjs')) {
  const mode = process.argv[2]
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  const input = JSON.parse(decodeChunks(chunks) || '{}')

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
