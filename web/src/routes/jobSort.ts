import type { Job } from '../api/types';

export type JobSortKey = 'createdAt' | 'updatedAt' | 'transferOrder';
export type SortDirection = 'asc' | 'desc';

type TimestampKey = 'createdAt' | 'updatedAt';

// Both timestamp fields are formatted server-side with timeFormat =
// "2006-01-02T15:04:05Z07:00" (internal/observ/observ.go). Plain string
// comparison only preserves chronological order if every value carries the
// *same* offset — two equal instants written with different offsets compare
// unequal, and an earlier instant can even sort after a later one across an
// offset change. That precondition holds here: created_at/updated_at are
// TIMESTAMPTZ and the deployment runs TZ=Etc/UTC (testenv/compose.yml), so
// every value the backend renders ends in "Z". No Date parsing is needed,
// and none should be added on the strength of that guarantee.
function compareTimestamp(a: Job, b: Job, key: TimestampKey): number {
  return a[key] < b[key] ? -1 : a[key] > b[key] ? 1 : 0;
}

// Rank used by 'transferOrder' below. Only 'active' and 'stalled' are
// meaningful here — the Overview TRANSFERS panel is the only caller, and it
// pre-filters to exactly those two statuses — so anything else is an
// unreachable third tier rather than a case this key needs to distinguish.
const TRANSFER_STATUS_RANK: Partial<Record<Job['status'], number>> = {
  active: 0,
  stalled: 1,
};

// 'transferOrder' exists for the TRANSFERS panel specifically (#233): status
// group first — active ranks above stalled — THEN createdAt ascending within
// a group. The group comes first, not age, because with max_active well
// above MAX_TRANSFER_ROWS (config.example.toml), the active|stalled set
// routinely exceeds the panel's row cap; a stalled job is old by
// construction and stays stalled, so pure age-ordering would let it pin a
// slot forever and hide active jobs that started later. Grouping by status
// means a row only moves in the panel when its status actually changes —
// real information — never merely because it aged relative to another row.
function compareTransferOrder(a: Job, b: Job): number {
  const rankA = TRANSFER_STATUS_RANK[a.status] ?? 2;
  const rankB = TRANSFER_STATUS_RANK[b.status] ?? 2;
  if (rankA !== rankB) return rankA - rankB;
  return compareTimestamp(a, b, 'createdAt');
}

// Keyed registry rather than a hardcoded switch/`.sort()` inline at each call
// site: a future sortable-column picker (separate issue) can add a key here
// as one entry instead of touching every place jobs get sorted.
const comparators: Record<JobSortKey, (a: Job, b: Job) => number> = {
  createdAt: (a, b) => compareTimestamp(a, b, 'createdAt'),
  updatedAt: (a, b) => compareTimestamp(a, b, 'updatedAt'),
  transferOrder: compareTransferOrder,
};

/**
 * Returns a new array of jobs sorted by `key`, in `direction`, with jobs
 * sharing the same timestamp broken by `id` ascending.
 *
 * Two properties are load-bearing, not incidental:
 *
 * - The input array is never mutated. `jobs` is typically an array held
 *   directly in the React Query all-jobs cache; sorting in place
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
