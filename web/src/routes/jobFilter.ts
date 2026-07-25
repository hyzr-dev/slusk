import type { Job, JobSource, JobStatus } from '../api/types';

export type SourceFilter = 'all' | JobSource;

// The status chip row needs one more bucket than JobStatus offers: "Importing"
// picks out the IMPORTING state, which dashboardStatus() (internal/observ/
// status.go) folds into the coarser "active" status alongside plain
// downloading jobs. It is therefore not a JobStatus value — it's a state-level
// refinement of "active" — so the chip filter is its own union rather than
// JobStatus itself, and "active" excludes IMPORTING jobs so the two chips
// never double-count the same job.
export type StatusFilter = 'all' | JobStatus | 'importing';

// Semantics preserved from the legacy dashboard: the whole search term is one
// case-insensitive substring, matched against each of "#id", title, artist and
// peer independently (a match in any one field is enough). No tokenisation.
//
// This must NOT be implemented as a single haystack of those fields joined by
// spaces: e.g. a job titled "Kind of Blue" by "Miles Davis" would then expose
// "...blue miles..." as a contiguous substring at the title/artist boundary,
// so searching "Blue Miles" would wrongly match. Per-field checks avoid that.
function matchesSearch(job: Job, search: string): boolean {
  if (!search) return true;
  const needle = search.toLowerCase();
  const fields = [`#${job.id}`, job.title, job.artist, job.peer];
  return fields.some((field) => field.toLowerCase().includes(needle));
}

function matchesSource(job: Job, source: SourceFilter): boolean {
  return source === 'all' || job.source === source;
}

function matchesStatus(job: Job, status: StatusFilter): boolean {
  if (status === 'all') return true;
  if (status === 'importing') return job.state === 'IMPORTING';
  if (status === 'active') return job.status === 'active' && job.state !== 'IMPORTING';
  return job.status === status;
}

export function matchesFilters(
  job: Job,
  search: string,
  status: StatusFilter,
  source: SourceFilter = 'all',
): boolean {
  if (!matchesStatus(job, status)) return false;
  if (!matchesSource(job, source)) return false;
  return matchesSearch(job, search);
}

export type StatusCounts = Record<JobStatus, number> & { importing: number };

// Counts per status (plus the synthetic "importing" bucket), used by the
// status chip counters. Deliberately ignores the status filter itself —
// otherwise selecting one chip would zero out every other chip's count — but
// respects source and search, so the counts reflect what a chip would show
// if clicked.
export function countByStatus(
  jobs: Job[],
  search: string,
  source: SourceFilter = 'all',
): StatusCounts {
  const counts: StatusCounts = {
    queued: 0,
    active: 0,
    stalled: 0,
    done: 0,
    failed: 0,
    orphaned: 0,
    importing: 0,
  };
  for (const job of jobs) {
    if (!matchesSource(job, source) || !matchesSearch(job, search)) continue;
    if (job.state === 'IMPORTING') {
      counts.importing += 1;
    } else {
      counts[job.status] += 1;
    }
  }
  return counts;
}
