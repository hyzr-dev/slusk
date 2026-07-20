import { describe, expect, it } from 'vitest';
import type { Job } from '../api/types';
import { matchesFilters } from './jobFilter';

const job = {
  id: 42,
  title: 'Kind of Blue',
  artist: 'Miles Davis',
  peer: 'someuser',
  status: 'active',
} as Job;

describe('matchesFilters', () => {
  it('matches everything when both filters are empty', () => {
    expect(matchesFilters(job, '', '')).toBe(true);
  });

  it('matches case-insensitively across title, artist and peer', () => {
    expect(matchesFilters(job, 'miles', '')).toBe(true);
    expect(matchesFilters(job, 'BLUE', '')).toBe(true);
    expect(matchesFilters(job, 'someuser', '')).toBe(true);
  });

  // The id is searchable with its # prefix, as in the legacy dashboard.
  it('matches the id including the hash prefix', () => {
    expect(matchesFilters(job, '#42', '')).toBe(true);
    expect(matchesFilters(job, '42', '')).toBe(true);
  });

  it('treats the search term as one substring, not as separate words', () => {
    expect(matchesFilters(job, 'Blue Miles', '')).toBe(false);
  });

  it('matches status exactly', () => {
    expect(matchesFilters(job, '', 'active')).toBe(true);
    expect(matchesFilters(job, '', 'queued')).toBe(false);
  });

  it('requires both filters to match', () => {
    expect(matchesFilters(job, 'miles', 'queued')).toBe(false);
  });
});
