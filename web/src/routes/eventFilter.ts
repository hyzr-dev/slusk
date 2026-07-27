import type { JobEvent } from '../api/types';

// Semantics preserved from the legacy dashboard: unlike the server-side job
// search's per-field matching, this joins the raw event code, detail text and
// job id into a single haystack and does one case-insensitive substring match.
//
// This cross-field behavior is INTENTIONAL: the two filters are deliberately
// different, and events keeps the legacy haystack behavior. See spec
// 2026-07-20.
export function matchesFilter(event: JobEvent, filter: string): boolean {
  if (!filter) return true;

  const haystack = `${event.event} ${event.detail} ${event.jobId}`.toLowerCase();
  return haystack.includes(filter.toLowerCase());
}
