import { describe, expect, it } from 'vitest';
import type { JobEvent } from '../api/types';
import { matchesFilter } from './eventFilter';

const event: JobEvent = {
  id: 7,
  jobId: 42,
  event: 'attempt_failed',
  detail: 'peer disconnected mid-transfer',
  createdAt: '2026-07-20T10:00:00Z',
};

describe('matchesFilter', () => {
  it('matches everything when the filter is empty', () => {
    expect(matchesFilter(event, '')).toBe(true);
  });

  it('matches case-insensitively', () => {
    expect(matchesFilter(event, 'DISCONNECTED')).toBe(true);
  });

  it('matches the raw event code, not the translated display label', () => {
    expect(matchesFilter(event, 'attempt_failed')).toBe(true);
    expect(matchesFilter(event, 'Attempt failed')).toBe(false);
  });

  it('matches on detail text', () => {
    expect(matchesFilter(event, 'mid-transfer')).toBe(true);
  });

  it('matches on job id', () => {
    expect(matchesFilter(event, '42')).toBe(true);
  });

  // Intentional divergence from jobFilter: events keeps the legacy cross-field
  // haystack behavior, so a filter spanning the joined boundary between two
  // fields still matches — jobFilter's per-field matching was deliberately
  // changed to reject the equivalent case. Do not "fix" this to be consistent
  // with jobFilter.
  it('matches across the joined event/detail boundary (intentional cross-field behavior)', () => {
    expect(matchesFilter(event, 'failed peer')).toBe(true);
  });
});
