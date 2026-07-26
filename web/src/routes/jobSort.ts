import type { Job } from '../api/types';

export type JobSortKey = 'createdAt' | 'updatedAt';
export type SortDirection = 'asc' | 'desc';

// Both timestamp fields are formatted server-side with timeFormat =
// "2006-01-02T15:04:05Z07:00" (internal/observ/observ.go) — ISO-8601 with a
// fixed numeric offset, which sorts correctly with plain string comparison.
// No Date parsing is needed, and none should be added: it would be pure
// overhead for a comparison that string comparison already gets right.
function compareTimestamp(a: Job, b: Job, key: JobSortKey): number {
  return a[key] < b[key] ? -1 : a[key] > b[key] ? 1 : 0;
}

// Keyed registry rather than a hardcoded switch/`.sort()` inline at each call
// site: a future sortable-column picker (separate issue) can add a key here
// as one entry instead of touching every place jobs get sorted.
const comparators: Record<JobSortKey, (a: Job, b: Job) => number> = {
  createdAt: (a, b) => compareTimestamp(a, b, 'createdAt'),
  updatedAt: (a, b) => compareTimestamp(a, b, 'updatedAt'),
};

/**
 * Returns a new array of jobs sorted by `key`, in `direction`, with jobs
 * sharing the same timestamp broken by `id` ascending.
 *
 * Two properties are load-bearing, not incidental:
 *
 * - The input array is never mutated. `jobs` is typically the array held
 *   directly in the React Query cache (`useJobs().data`); sorting in place
 *   would rewrite that shared cache and silently change the order every other
 *   consumer of the same query sees. This always builds a fresh array first.
 *
 * - The `id` tie-break is always ascending, regardless of `direction`. The
 *   backend timestamps have only second resolution, so two jobs updated in
 *   the same poll tick can compare equal. Array.prototype.sort is stable, so
 *   without an explicit tie-break those jobs would keep whatever relative
 *   order they arrived in from the backend's `ORDER BY updated_at DESC` — an
 *   order that itself isn't stable across polls — and they'd visibly swap
 *   positions. Keeping the tie-break's direction fixed (rather than flipping
 *   it with `direction`) matters too: if it flipped, switching to descending
 *   order would itself reshuffle same-second rows relative to ascending.
 */
export function sortJobs(jobs: Job[], key: JobSortKey, direction: SortDirection): Job[] {
  const compare = comparators[key];
  const sign = direction === 'asc' ? 1 : -1;
  return [...jobs].sort((a, b) => {
    const primary = compare(a, b);
    if (primary !== 0) return primary * sign;
    return a.id - b.id;
  });
}
