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
