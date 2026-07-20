import type { JobEvent } from '../api/types';

// Semantics preserved from the legacy dashboard: unlike jobFilter's per-field
// matching, this joins the raw event code, the detail text and the job id
// into a single haystack and does one case-insensitive substring match
// against it.
//
// This cross-field behavior is INTENTIONAL and must not be "fixed" to match
// jobFilter's per-field approach — the two filters are deliberately
// different (jobFilter was changed to per-field matching specifically to
// avoid this kind of boundary match; events keeps the legacy haystack
// behavior). See spec 2026-07-20.
export function matchesFilter(event: JobEvent, filter: string): boolean {
  if (!filter) return true;

  const haystack = `${event.event} ${event.detail} ${event.jobId}`.toLowerCase();
  return haystack.includes(filter.toLowerCase());
}
