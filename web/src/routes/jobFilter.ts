import type { Job } from '../api/types';

// Semantics preserved from the legacy dashboard: the whole search term is one
// case-insensitive substring, matched against each of "#id", title, artist and
// peer independently (a match in any one field is enough). No tokenisation.
//
// This must NOT be implemented as a single haystack of those fields joined by
// spaces: e.g. a job titled "Kind of Blue" by "Miles Davis" would then expose
// "...blue miles..." as a contiguous substring at the title/artist boundary,
// so searching "Blue Miles" would wrongly match. Per-field checks avoid that.
export function matchesFilters(job: Job, search: string, status: string): boolean {
  if (status && job.status !== status) return false;
  if (!search) return true;

  const needle = search.toLowerCase();
  const fields = [`#${job.id}`, job.title, job.artist, job.peer];
  return fields.some((field) => field.toLowerCase().includes(needle));
}
