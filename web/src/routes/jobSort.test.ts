import { describe, expect, it } from 'vitest';
import type { Job } from '../api/types';
import { sortJobs } from './jobSort';

const job = {
  id: 1,
  title: 'Kind of Blue',
  artist: 'Miles Davis',
  peer: 'someuser',
  status: 'active',
  state: 'DOWNLOADING',
  source: 'lidarr',
  createdAt: '2026-07-01T10:00:00Z',
  updatedAt: '2026-07-01T10:00:00Z',
} as Job;

describe('sortJobs', () => {
  it('sorts by createdAt ascending, oldest first', () => {
    const jobs: Job[] = [
      { ...job, id: 1, createdAt: '2026-07-01T12:00:00Z' },
      { ...job, id: 2, createdAt: '2026-07-01T10:00:00Z' },
      { ...job, id: 3, createdAt: '2026-07-01T11:00:00Z' },
    ];
    expect(sortJobs(jobs, 'createdAt', 'asc').map((j) => j.id)).toEqual([2, 3, 1]);
  });

  it('sorts by createdAt descending, newest first', () => {
    const jobs: Job[] = [
      { ...job, id: 1, createdAt: '2026-07-01T12:00:00Z' },
      { ...job, id: 2, createdAt: '2026-07-01T10:00:00Z' },
      { ...job, id: 3, createdAt: '2026-07-01T11:00:00Z' },
    ];
    expect(sortJobs(jobs, 'createdAt', 'desc').map((j) => j.id)).toEqual([1, 3, 2]);
  });

  it('sorts by updatedAt independently of createdAt', () => {
    const jobs: Job[] = [
      { ...job, id: 1, createdAt: '2026-07-01T10:00:00Z', updatedAt: '2026-07-01T12:00:00Z' },
      { ...job, id: 2, createdAt: '2026-07-01T11:00:00Z', updatedAt: '2026-07-01T09:00:00Z' },
    ];
    expect(sortJobs(jobs, 'updatedAt', 'asc').map((j) => j.id)).toEqual([2, 1]);
  });

  // The backend timestamp has second resolution, so two jobs can share the
  // same createdAt; they must tie-break on id ascending so the order doesn't
  // flap between polls even though ORDER BY updated_at DESC on the backend
  // is itself unstable across ties.
  it('tie-breaks on id ascending when timestamps are equal', () => {
    const jobs: Job[] = [
      { ...job, id: 5, createdAt: '2026-07-01T10:00:00Z' },
      { ...job, id: 2, createdAt: '2026-07-01T10:00:00Z' },
      { ...job, id: 8, createdAt: '2026-07-01T10:00:00Z' },
    ];
    expect(sortJobs(jobs, 'createdAt', 'asc').map((j) => j.id)).toEqual([2, 5, 8]);
  });

  // The id tie-break direction must stay fixed regardless of `direction` —
  // otherwise switching to descending order would itself reorder same-second
  // rows relative to ascending, which is exactly the reshuffling this fix
  // exists to eliminate.
  it('keeps the id tie-break ascending even when direction is descending', () => {
    const jobs: Job[] = [
      { ...job, id: 5, createdAt: '2026-07-01T10:00:00Z' },
      { ...job, id: 2, createdAt: '2026-07-01T10:00:00Z' },
      { ...job, id: 8, createdAt: '2026-07-01T10:00:00Z' },
    ];
    expect(sortJobs(jobs, 'createdAt', 'desc').map((j) => j.id)).toEqual([2, 5, 8]);
  });

  it('does not mutate the input array', () => {
    const jobs: Job[] = [
      { ...job, id: 1, createdAt: '2026-07-01T12:00:00Z' },
      { ...job, id: 2, createdAt: '2026-07-01T10:00:00Z' },
    ];
    const original = [...jobs];
    sortJobs(jobs, 'createdAt', 'asc');
    expect(jobs).toEqual(original);
    expect(jobs.map((j) => j.id)).toEqual([1, 2]);
  });

  describe('transferOrder', () => {
    // The entire point of this key (#233): a stalled job is old by
    // construction and stays stalled, so ranking by age alone would let it
    // permanently occupy a slot in a capped panel. Status group must win
    // over age, even when it's the stalled job that's far older.
    it('ranks active before stalled regardless of age', () => {
      const jobs: Job[] = [
        { ...job, id: 1, status: 'stalled', createdAt: '2020-01-01T00:00:00Z' },
        { ...job, id: 2, status: 'active', createdAt: '2026-07-01T12:00:00Z' },
      ];
      expect(sortJobs(jobs, 'transferOrder', 'asc').map((j) => j.id)).toEqual([2, 1]);
    });

    it('orders by createdAt ascending within the active group', () => {
      const jobs: Job[] = [
        { ...job, id: 1, status: 'active', createdAt: '2026-07-01T12:00:00Z' },
        { ...job, id: 2, status: 'active', createdAt: '2026-07-01T10:00:00Z' },
        { ...job, id: 3, status: 'active', createdAt: '2026-07-01T11:00:00Z' },
      ];
      expect(sortJobs(jobs, 'transferOrder', 'asc').map((j) => j.id)).toEqual([2, 3, 1]);
    });

    it('orders by createdAt ascending within the stalled group', () => {
      const jobs: Job[] = [
        { ...job, id: 1, status: 'stalled', createdAt: '2026-07-01T12:00:00Z' },
        { ...job, id: 2, status: 'stalled', createdAt: '2026-07-01T10:00:00Z' },
        { ...job, id: 3, status: 'stalled', createdAt: '2026-07-01T11:00:00Z' },
      ];
      expect(sortJobs(jobs, 'transferOrder', 'asc').map((j) => j.id)).toEqual([2, 3, 1]);
    });

    it('tie-breaks on id ascending within a group when createdAt is equal', () => {
      const jobs: Job[] = [
        { ...job, id: 5, status: 'active', createdAt: '2026-07-01T10:00:00Z' },
        { ...job, id: 2, status: 'active', createdAt: '2026-07-01T10:00:00Z' },
        { ...job, id: 8, status: 'active', createdAt: '2026-07-01T10:00:00Z' },
      ];
      expect(sortJobs(jobs, 'transferOrder', 'asc').map((j) => j.id)).toEqual([2, 5, 8]);
    });

    it('interleaves both rules: active-first, then age within each group', () => {
      const jobs: Job[] = [
        { ...job, id: 1, status: 'stalled', createdAt: '2026-07-01T09:00:00Z' },
        { ...job, id: 2, status: 'active', createdAt: '2026-07-01T12:00:00Z' },
        { ...job, id: 3, status: 'active', createdAt: '2026-07-01T11:00:00Z' },
        { ...job, id: 4, status: 'stalled', createdAt: '2026-07-01T09:30:00Z' },
      ];
      expect(sortJobs(jobs, 'transferOrder', 'asc').map((j) => j.id)).toEqual([3, 2, 1, 4]);
    });
  });
});
