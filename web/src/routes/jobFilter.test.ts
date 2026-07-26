import { describe, expect, it } from 'vitest';
import type { Job } from '../api/types';
import { countByStatus, matchesFilters } from './jobFilter';

const job = {
  id: 42,
  title: 'Kind of Blue',
  artist: 'Miles Davis',
  peer: 'someuser',
  status: 'active',
  state: 'DOWNLOADING',
  source: 'lidarr',
} as Job;

describe('matchesFilters', () => {
  it('matches everything when both filters are empty', () => {
    expect(matchesFilters(job, '', 'all')).toBe(true);
  });

  it('matches case-insensitively across title, artist and peer', () => {
    expect(matchesFilters(job, 'miles', 'all')).toBe(true);
    expect(matchesFilters(job, 'BLUE', 'all')).toBe(true);
    expect(matchesFilters(job, 'someuser', 'all')).toBe(true);
  });

  // The id is searchable with its # prefix, as in the legacy dashboard.
  it('matches the id including the hash prefix', () => {
    expect(matchesFilters(job, '#42', 'all')).toBe(true);
    expect(matchesFilters(job, '42', 'all')).toBe(true);
  });

  it('treats the search term as one substring, not as separate words', () => {
    expect(matchesFilters(job, 'Blue Miles', 'all')).toBe(false);
  });

  it('matches status exactly', () => {
    expect(matchesFilters(job, '', 'active')).toBe(true);
    expect(matchesFilters(job, '', 'queued')).toBe(false);
  });

  it('requires both filters to match', () => {
    expect(matchesFilters(job, 'miles', 'queued')).toBe(false);
  });

  it('matches source exactly', () => {
    expect(matchesFilters(job, '', 'all', 'lidarr')).toBe(true);
    expect(matchesFilters(job, '', 'all', 'manual')).toBe(false);
    expect(matchesFilters(job, '', 'all', 'all')).toBe(true);
  });

  it('requires source, status and search to all match', () => {
    expect(matchesFilters(job, 'miles', 'active', 'lidarr')).toBe(true);
    expect(matchesFilters(job, 'miles', 'active', 'manual')).toBe(false);
  });

  it('treats the "importing" status filter as a state refinement, not a JobStatus', () => {
    const importing = { ...job, status: 'active', state: 'IMPORTING' } as Job;
    const downloading = { ...job, status: 'active', state: 'DOWNLOADING' } as Job;
    expect(matchesFilters(importing, '', 'importing')).toBe(true);
    expect(matchesFilters(downloading, '', 'importing')).toBe(false);
    // "active" excludes IMPORTING jobs so the two chips never double-count.
    expect(matchesFilters(importing, '', 'active')).toBe(false);
    expect(matchesFilters(downloading, '', 'active')).toBe(true);
  });
});

describe('countByStatus', () => {
  const jobs: Job[] = [
    { ...job, id: 1, status: 'active', state: 'DOWNLOADING', source: 'lidarr' } as Job,
    { ...job, id: 2, status: 'active', state: 'IMPORTING', source: 'manual' } as Job,
    { ...job, id: 3, status: 'queued', state: 'WANTED', source: 'lidarr' } as Job,
    { ...job, id: 4, status: 'failed', state: 'FAILED', source: 'manual' } as Job,
  ];

  it('buckets each job into exactly one status, splitting importing out of active', () => {
    const counts = countByStatus(jobs, '', 'all');
    expect(counts).toEqual({
      queued: 1,
      active: 1,
      stalled: 0,
      done: 0,
      failed: 1,
      parked: 0,
      importing: 1,
    });
  });

  it('respects the source filter', () => {
    const counts = countByStatus(jobs, '', 'manual');
    expect(counts.importing).toBe(1);
    expect(counts.active).toBe(0);
    expect(counts.failed).toBe(1);
    expect(counts.queued).toBe(0);
  });

  it('respects the search text', () => {
    const withTitles: Job[] = [
      { ...job, id: 1, title: 'Kind of Blue', status: 'active', state: 'DOWNLOADING' } as Job,
      { ...job, id: 2, title: 'Rounds', status: 'queued', state: 'WANTED' } as Job,
    ];
    const counts = countByStatus(withTitles, 'rounds', 'all');
    expect(counts.queued).toBe(1);
    expect(counts.active).toBe(0);
  });

  it('every bucket sums to the number of jobs matching source and search — the "All" chip invariant', () => {
    // countByStatus takes no status argument at all, so a caller cannot zero
    // out other chips' counts by passing the currently-selected status —
    // and summing every bucket must equal exactly what matchesFilters(..., 'all', source)
    // would return, which is what the "All" chip's own count needs to show.
    const counts = countByStatus(jobs, '', 'lidarr');
    const total = Object.values(counts).reduce((sum, n) => sum + n, 0);
    const matchingAll = jobs.filter((j) => matchesFilters(j, '', 'all', 'lidarr')).length;
    expect(total).toBe(matchingAll);
    expect(total).toBe(2);
  });
});
