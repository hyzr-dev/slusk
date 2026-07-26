import { describe, expect, it } from 'vitest';
import type { WireJob, WireStatusReport } from './types';
import { normalizeJobDetail, normalizeJobs, normalizeStatusReport } from './normalize';

function wireJob(overrides: Partial<WireJob> = {}): WireJob {
  return {
    id: 1,
    title: 'Kind of Blue',
    artist: 'Miles Davis',
    status: 'active',
    peer: '',
    bytesDone: 0,
    bytesTotal: 0,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    state: 'DOWNLOADING',
    candidatesTried: 0,
    maxCandidates: 3,
    failReason: '',
    nextAttemptAt: '',
    retries: 0,
    notBefore: '',
    source: 'lidarr',
    year: null,
    tracks: null,
    format: null,
    ...overrides,
  };
}

function wireStatus(overrides: Partial<WireStatusReport> = {}): WireStatusReport {
  return {
    queued: 1,
    active: 2,
    stalled: 3,
    modules: {},
    moduleDetails: {},
    ...overrides,
  };
}

describe('wire compatibility normalization', () => {
  it('normalizes old job-list and detail payloads to canonical parked values', () => {
    const [job] = normalizeJobs([
      wireJob({ status: 'orphaned', state: 'ORPHANED' }),
    ]);
    const detail = normalizeJobDetail({
      id: 1,
      title: 'Kind of Blue',
      artist: 'Miles Davis',
      state: 'ORPHANED',
      attempts: [],
    });

    expect(job).toMatchObject({ status: 'parked', state: 'PARKED' });
    expect(detail.state).toBe('PARKED');
  });

  it('accepts new and mixed job payloads without exposing a legacy UI value', () => {
    const jobs = normalizeJobs([
      wireJob({ id: 1, status: 'parked', state: 'PARKED' }),
      wireJob({ id: 2, status: 'parked', state: 'ORPHANED' }),
    ]);

    expect(jobs.map(({ status, state }) => ({ status, state }))).toEqual([
      { status: 'parked', state: 'PARKED' },
      { status: 'parked', state: 'PARKED' },
    ]);
  });

  it('falls back to orphaned for an old status payload', () => {
    expect(normalizeStatusReport(wireStatus({ orphaned: 4 })).parked).toBe(4);
  });

  it('prefers parked when a mixed status payload contains differing counts', () => {
    const normalized = normalizeStatusReport(wireStatus({ parked: 5, orphaned: 99 }));

    expect(normalized.parked).toBe(5);
    expect(normalized).not.toHaveProperty('orphaned');
  });
});
