import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  pickJobDetail,
  mergeLiveJobs,
  mergeThroughputSamples,
  queryKeys,
  useJobDetail,
  useJobs,
} from './queries';
import type { Job, JobDetail, ScopedLivePayload, ThroughputSample } from './types';

afterEach(() => vi.unstubAllGlobals());

function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: 1,
    title: 'Kind of Blue',
    artist: 'Miles Davis',
    status: 'active',
    peer: 'someuser',
    bytesDone: 50,
    bytesTotal: 100,
    createdAt: '2026-07-01T10:00:00Z',
    updatedAt: '2026-07-01T10:00:00Z',
    state: 'DOWNLOADING',
    candidatesTried: 1,
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

function makeDetail(overrides: Partial<JobDetail> = {}): JobDetail {
  return {
    id: 1,
    title: 'Kind of Blue',
    artist: 'Miles Davis',
    state: 'DOWNLOADING',
    attempts: [
      {
        id: 1,
        username: 'someuser',
        fileCount: 2,
        state: 'ACTIVE',
        failReason: '',
        createdAt: '2026-07-01T10:00:00Z',
        updatedAt: '2026-07-01T10:00:00Z',
        transfers: [
          { filename: '01.flac', state: 'IN_PROGRESS', bytesDone: 400, bytesTotal: 1000, retries: 0, lastProgressAt: '' },
          { filename: '02.flac', state: 'QUEUED', bytesDone: 0, bytesTotal: 800, retries: 0, lastProgressAt: '' },
        ],
      },
    ],
    ...overrides,
  };
}

describe('mergeLiveJobs', () => {
  it('overlays live fields onto the matching REST job', () => {
    const jobs = [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100 })];
    const merged = mergeLiveJobs(jobs, [
      { id: 1, bytesDone: 60, bytesTotal: 100, speed: 5000, queuePosition: 1, etaSeconds: 12 },
    ]);
    expect(merged?.[0]).toMatchObject({ bytesDone: 60, speed: 5000, queuePosition: 1, etaSeconds: 12 });
  });

  // The core #161 contract: a job present in one frame and absent from the
  // next must revert to its REST-cached values, not keep a stale live speed.
  it('leaves a job absent from the live set untouched (reverts to REST values)', () => {
    const jobs = [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100 })];
    // job 1 was live a moment ago...
    const withLive = mergeLiveJobs(jobs, [{ id: 1, bytesDone: 90, bytesTotal: 100, speed: 9000 }]);
    expect(withLive?.[0].speed).toBe(9000);
    // ...then drops out of the next frame entirely.
    const afterDrop = mergeLiveJobs(jobs, []);
    expect(afterDrop).toBe(jobs); // untouched REST data, no lingering speed
    expect(afterDrop?.[0].speed).toBeUndefined();
  });

  it('passes through jobs unchanged when live data is undefined', () => {
    const jobs = [makeJob()];
    expect(mergeLiveJobs(jobs, undefined)).toBe(jobs);
  });
});

describe('pickJobDetail', () => {
  // The stream's detail is built by the same server-side function as REST's,
  // so it is adopted whole rather than merged field by field (issue #258).
  it('replaces the REST detail with the stream\'s when scoped to this job', () => {
    const rest = makeDetail();
    const streamed = makeDetail();
    streamed.attempts[0].transfers[0] = {
      filename: '01.flac',
      state: 'IN_PROGRESS',
      bytesDone: 450,
      bytesTotal: 1000,
      retries: 0,
      lastProgressAt: '',
      speed: 2000,
    };
    const live: ScopedLivePayload = { jobs: [], down: 0, scopeJobId: 1, detail: streamed };
    expect(pickJobDetail(rest, live, 1)).toBe(streamed);
  });

  // The regression the merge kept producing: a finished transfer flipping back
  // to downloading. With a whole-object replace it cannot happen — whichever
  // object is shown was built in one pass by one authority, so its state and
  // its speed always agree with each other.
  it('never shows a terminal transfer together with a speed', () => {
    const rest = makeDetail();
    const streamed = makeDetail();
    streamed.attempts[0].transfers[0] = {
      filename: '01.flac',
      state: 'COMPLETED',
      bytesDone: 1000,
      bytesTotal: 1000,
      retries: 0,
      lastProgressAt: '',
    };
    const live: ScopedLivePayload = { jobs: [], down: 0, scopeJobId: 1, detail: streamed };
    const tr = pickJobDetail(rest, live, 1)?.attempts[0].transfers[0];
    expect(tr?.state).toBe('COMPLETED');
    expect(tr?.speed).toBeUndefined();
  });

  it('ignores a frame scoped to a different job (stale during a route change)', () => {
    const rest = makeDetail();
    const other = makeDetail();
    other.id = 2;
    const live: ScopedLivePayload = { jobs: [], down: 0, scopeJobId: 2, detail: other };
    expect(pickJobDetail(rest, live, 1)).toBe(rest);
  });

  // An unscoped connection (the /jobs list, where JobExpansion renders the
  // same transfers inline) carries no detail at all, so REST stays in charge.
  it('falls back to REST when the frame carries no detail', () => {
    const rest = makeDetail();
    const live: ScopedLivePayload = { jobs: [], down: 0, scopeJobId: 1 };
    expect(pickJobDetail(rest, live, 1)).toBe(rest);
  });

  it('returns the REST detail unchanged when there is no live data', () => {
    const rest = makeDetail();
    expect(pickJobDetail(rest, undefined, 1)).toBe(rest);
    expect(pickJobDetail(rest, null, 1)).toBe(rest);
  });
});

describe('mergeThroughputSamples', () => {
  it('appends new samples and dedupes on `at`', () => {
    const existing: ThroughputSample[] = [
      { at: '2026-07-26T12:00:00Z', bytesPerSecond: 1000, activeTransfers: 1 },
      { at: '2026-07-26T12:00:01Z', bytesPerSecond: 1100, activeTransfers: 1 },
    ];
    const incoming: ThroughputSample[] = [
      { at: '2026-07-26T12:00:01Z', bytesPerSecond: 1100, activeTransfers: 1 }, // duplicate
      { at: '2026-07-26T12:00:02Z', bytesPerSecond: 1200, activeTransfers: 2 },
    ];
    const merged = mergeThroughputSamples(existing, incoming);
    expect(merged.map((s) => s.at)).toEqual([
      '2026-07-26T12:00:00Z',
      '2026-07-26T12:00:01Z',
      '2026-07-26T12:00:02Z',
    ]);
  });

  it('caps the series length rather than growing it unbounded', () => {
    const existing: ThroughputSample[] = Array.from({ length: 48 }, (_, i) => ({
      at: `2026-07-26T12:00:${String(i).padStart(2, '0')}Z`,
      bytesPerSecond: i,
      activeTransfers: 1,
    }));
    const merged = mergeThroughputSamples(existing, [
      { at: '2026-07-26T12:01:00Z', bytesPerSecond: 999, activeTransfers: 1 },
    ]);
    expect(merged).toHaveLength(48);
    expect(merged.at(-1)?.at).toBe('2026-07-26T12:01:00Z');
    expect(merged[0].at).toBe('2026-07-26T12:00:01Z'); // oldest sample fell off
  });
});

function makeWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

describe('useJobs live overlay', () => {
  it('merges live fields into the hook output as the live cache changes', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const jobs = [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100 })];
    queryClient.setQueryData(queryKeys.jobs, jobs);

    const { result, rerender } = renderHook(() => useJobs(), { wrapper: makeWrapper(queryClient) });
    expect(result.current.data?.[0].speed).toBeUndefined();

    queryClient.setQueryData(queryKeys.live, { jobs: [{ id: 1, bytesDone: 80, bytesTotal: 100, speed: 4000 }], down: 4000 });
    rerender();
    expect(result.current.data?.[0]).toMatchObject({ bytesDone: 80, speed: 4000 });

    // Job 1 drops out of the next frame — reverts to REST values. The
    // overlay is never written back into the jobs cache, so what comes back
    // is the pristine REST object, which has no `speed` key at all (hence
    // the per-property assertion — see the note in pickJobDetail's test).
    queryClient.setQueryData(queryKeys.live, { jobs: [], down: 0 });
    rerender();
    expect(result.current.data?.[0].bytesDone).toBe(50);
    expect(result.current.data?.[0].speed).toBeUndefined();
  });
});

describe('useJobDetail live detail', () => {
  it('serves the stream\'s scoped detail in place of the REST one', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(queryKeys.jobDetail(1), makeDetail());

    const { result, rerender } = renderHook(() => useJobDetail(1), { wrapper: makeWrapper(queryClient) });
    expect(result.current.data?.attempts[0].transfers[0].speed).toBeUndefined();

    const streamed = makeDetail();
    streamed.attempts[0].transfers[0] = {
      filename: '01.flac',
      state: 'IN_PROGRESS',
      bytesDone: 500,
      bytesTotal: 1000,
      retries: 0,
      lastProgressAt: '',
      speed: 3000,
    };
    queryClient.setQueryData(queryKeys.live, { jobs: [], down: 0, scopeJobId: 1, detail: streamed });
    rerender();
    expect(result.current.data?.attempts[0].transfers[0]).toMatchObject({ bytesDone: 500, speed: 3000 });

    // Stream drops: the hook falls straight back to the REST object still in
    // the cache, because the stream's detail was never written into it.
    queryClient.setQueryData(queryKeys.live, null);
    rerender();
    expect(result.current.data?.attempts[0].transfers[0].bytesDone).toBe(400);
    expect(result.current.data?.attempts[0].transfers[0].speed).toBeUndefined();
  });
});
