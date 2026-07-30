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
