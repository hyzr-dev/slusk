import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_JOB_PAGE_PARAMS,
  jobsPageUrl,
  replaceLiveJobPage,
  pickJobDetail,
  replaceLiveJobs,
  mergeThroughputSamples,
  queryKeys,
  useJobDetail,
  useJobs,
} from './queries';
import type { Job, JobDetail, JobPage, ScopedLivePayload, ThroughputSample, WireJob } from './types';

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

// jobDetailDTO carries a whole jobDTO under `job` (issue #268) rather than a
// hand-picked subset of fields — see the JobDetail doc comment in types.ts.
function makeDetail(overrides: Partial<JobDetail> = {}): JobDetail {
  return {
    job: makeJob({ state: 'DOWNLOADING' }),
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

describe('replaceLiveJobs', () => {
  // Issue #258: the stream now carries full Job objects, and the client
  // adopts one wholesale rather than overlaying individual fields — a
  // streamed job replaces the REST row entirely, including fields the old
  // partial LiveJob never touched (retries, failReason, ...).
  it('replaces a job wholesale with its streamed counterpart', () => {
    const jobs = [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100, retries: 0 })];
    const streamed: WireJob = makeJob({ id: 1, bytesDone: 60, bytesTotal: 100, speed: 5000, queuePosition: 1, etaSeconds: 12, retries: 2 });
    const replaced = replaceLiveJobs(jobs, [streamed]);
    expect(replaced?.[0]).toEqual(streamed);
  });

  // A job absent from this frame simply wasn't touched this tick — it is
  // left exactly as REST last reported it, not reset or cleared.
  it('leaves a job absent from the live frame untouched', () => {
    const jobs = [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100 })];
    // job 1 changed a moment ago...
    const withLive = replaceLiveJobs(jobs, [makeJob({ id: 1, bytesDone: 90, bytesTotal: 100, speed: 9000 })]);
    expect(withLive?.[0].speed).toBe(9000);
    // ...then is absent from the next frame entirely (nothing changed for it).
    const afterDrop = replaceLiveJobs(jobs, []);
    expect(afterDrop).toBe(jobs); // untouched REST data, no lingering speed
    expect(afterDrop?.[0].speed).toBeUndefined();
  });

  it('passes through jobs unchanged when live data is undefined', () => {
    const jobs = [makeJob()];
    expect(replaceLiveJobs(jobs, undefined)).toBe(jobs);
  });

  // stream.tsx's accumulator never evicts an entry on its own (the backend
  // stops streaming a job once it leaves the live-matched set, including
  // post-terminal DB transitions that settle after the last live frame) — so
  // presence in `live` can't be the tie-breaker, or a REST poll could never
  // correct a job pinned at stale live values. `updatedAt` is: REST strictly
  // newer means the DB genuinely moved since the pinned live object was
  // built, so REST wins and the pin is released.
  it('lets a newer REST row override a pinned live object once the DB has moved on', () => {
    const pinned = makeJob({ id: 1, state: 'DOWNLOADING', status: 'active', speed: 0, updatedAt: '2026-07-27T10:00:00Z' });
    const restAfterReconcile = makeJob({ id: 1, state: 'DONE', status: 'done', updatedAt: '2026-07-27T10:00:15Z' });

    const replaced = replaceLiveJobs([restAfterReconcile], [pinned]);
    expect(replaced?.[0]).toEqual(restAfterReconcile);

    // A later poll with the same (already-correct) row must keep winning —
    // the pin was released, not just skipped once.
    const replacedAgain = replaceLiveJobs([restAfterReconcile], [pinned]);
    expect(replacedAgain?.[0]).toEqual(restAfterReconcile);
  });

  // The mirror image: DB unchanged (equal updatedAt) means only live fields
  // moved, so the live object still wins — this is the ordinary, expected
  // case the accumulator exists for.
  it('still prefers the live object when the REST row has not moved on (equal updatedAt)', () => {
    const jobs = [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100, updatedAt: '2026-07-27T10:00:00Z' })];
    const streamed = makeJob({ id: 1, bytesDone: 80, bytesTotal: 100, speed: 4000, updatedAt: '2026-07-27T10:00:00Z' });
    expect(replaceLiveJobs(jobs, [streamed])?.[0]).toEqual(streamed);
  });
});

describe('paged jobs transport', () => {
  it('isolates every request primitive in query keys and URL-encodes search values', () => {
    const params = { ...DEFAULT_JOB_PAGE_PARAMS, page: 2, sort: 'peer' as const, dir: 'desc' as const, filter: 'failed' as const, source: 'manual' as const, q: 'Miles & Blue' };
    const key = queryKeys.jobsPage(params);

    expect(key).toEqual(['jobs', 'page', 2, 'peer', 'desc', 'failed', 'manual', 'Miles & Blue', undefined]);
    expect(queryKeys.jobsPage({ ...params, source: 'lidarr' })).not.toEqual(key);
    expect(jobsPageUrl(params)).toBe('/api/jobs?page=2&sort=peer&dir=desc&filter=failed&source=manual&q=Miles+%26+Blue');
  });

  // Overview (issue #268) requests a smaller page than the paged Jobs route,
  // so pageSize has to isolate its own cache entry and reach the URL.
  it('sends pageSize only when given, and isolates it in the query key', () => {
    const withSize = { ...DEFAULT_JOB_PAGE_PARAMS, pageSize: 8 };
    expect(queryKeys.jobsPage(withSize)).toEqual(['jobs', 'page', 0, 'st', 'asc', 'all', 'all', '', 8]);
    expect(queryKeys.jobsPage(withSize)).not.toEqual(queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS));
    expect(jobsPageUrl(withSize)).toBe('/api/jobs?page=0&sort=st&dir=asc&filter=all&source=all&q=&pageSize=8');
    expect(jobsPageUrl(DEFAULT_JOB_PAGE_PARAMS)).not.toContain('pageSize');
  });

  it('overlays current-page IDs only while preserving order and metadata', () => {
    const page: JobPage = {
      jobs: [makeJob({ id: 2 }), makeJob({ id: 1 })],
      total: 25,
      facets: {
        status: { all: 25, active: 2, importing: 1, queued: 3, stalled: 4, failed: 5, parked: 6, done: 4 },
        source: { all: 25, manual: 5, lidarr: 20 },
      },
    };
    const merged = replaceLiveJobPage(page, [
      makeJob({ id: 1, bytesDone: 99, bytesTotal: 100, speed: 5000 }),
      makeJob({ id: 3, bytesDone: 10, bytesTotal: 20, speed: 1000 }),
    ]);

    expect(merged?.jobs.map((job) => job.id)).toEqual([2, 1]);
    expect(merged?.jobs[1]).toMatchObject({ id: 1, bytesDone: 99, speed: 5000 });
    expect(merged?.total).toBe(25);
    expect(merged?.facets).toBe(page.facets);
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
    const live: ScopedLivePayload = { jobs: [], down: 0, up: 0, scopeJobId: 1, detail: streamed };
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
    const live: ScopedLivePayload = { jobs: [], down: 0, up: 0, scopeJobId: 1, detail: streamed };
    const tr = pickJobDetail(rest, live, 1)?.attempts[0].transfers[0];
    expect(tr?.state).toBe('COMPLETED');
    expect(tr?.speed).toBeUndefined();
  });

  it('ignores a frame scoped to a different job (stale during a route change)', () => {
    const rest = makeDetail();
    const other = makeDetail();
    other.job.id = 2;
    const live: ScopedLivePayload = { jobs: [], down: 0, up: 0, scopeJobId: 2, detail: other };
    expect(pickJobDetail(rest, live, 1)).toBe(rest);
  });

  // An unscoped connection (the /jobs list, where JobExpansion renders the
  // same transfers inline) carries no detail at all, so REST stays in charge.
  it('falls back to REST when the frame carries no detail', () => {
    const rest = makeDetail();
    const live: ScopedLivePayload = { jobs: [], down: 0, up: 0, scopeJobId: 1 };
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

describe('useJobs live replace', () => {
  it('replaces jobs wholesale in the page hook output as the live cache changes', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const page: JobPage = {
      jobs: [makeJob({ id: 1, bytesDone: 50, bytesTotal: 100 })],
      total: 1,
      facets: {
        status: { all: 1, active: 1, importing: 0, queued: 0, stalled: 0, failed: 0, parked: 0, done: 0 },
        source: { all: 1, manual: 0, lidarr: 1 },
      },
    };
    queryClient.setQueryData(queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS), page);

    const { result, rerender } = renderHook(() => useJobs(DEFAULT_JOB_PAGE_PARAMS), { wrapper: makeWrapper(queryClient) });
    expect(result.current.data?.jobs[0].speed).toBeUndefined();

    queryClient.setQueryData(queryKeys.live, {
      jobs: [makeJob({ id: 1, bytesDone: 80, bytesTotal: 100, speed: 4000 })],
      down: 4000,
      up: 0,
    });
    rerender();
    expect(result.current.data?.jobs[0]).toMatchObject({ bytesDone: 80, speed: 4000 });

    // A truly empty live cache (no jobs at all) reverts every row to REST —
    // this is the post-reset/no-live-data-yet state (see stream.tsx's
    // clearLive), not what a delta frame that simply didn't mention a job
    // looks like. See the next describe block for that distinction.
    queryClient.setQueryData(queryKeys.live, { jobs: [], down: 0, up: 0 });
    rerender();
    expect(result.current.data?.jobs[0].bytesDone).toBe(50);
    expect(result.current.data?.jobs[0].speed).toBeUndefined();
    expect(result.current.data?.total).toBe(1);
  });
});

describe('useJobs live replace across accumulated frames', () => {
  // `jobs` on the wire is a per-tick delta of changed jobs, not a snapshot
  // (issue #258's fix-round bug). stream.tsx accumulates each frame by id
  // before ever writing queryKeys.live, so the cache replaceLiveJobs reads
  // always holds the *union* of every job changed since the connection's
  // last GET — never a bare single-frame delta. This test models that
  // accumulated cache directly (bypassing stream.tsx) to pin down the
  // contract: a job absent from the latest delta must keep the values an
  // earlier frame gave it, for as long as the accumulated cache still
  // carries it.
  it('keeps a job at its last-known live values when a later frame does not mention it', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const page: JobPage = {
      jobs: [
        makeJob({ id: 1, bytesDone: 10, bytesTotal: 100 }),
        makeJob({ id: 2, bytesDone: 44_000_000, bytesTotal: 49_000_000, speed: 1_000_000, state: 'DOWNLOADING' }),
      ],
      total: 2,
      facets: {
        status: { all: 2, active: 2, importing: 0, queued: 0, stalled: 0, failed: 0, parked: 0, done: 0 },
        source: { all: 2, manual: 0, lidarr: 2 },
      },
    };
    queryClient.setQueryData(queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS), page);

    const { result, rerender } = renderHook(() => useJobs(DEFAULT_JOB_PAGE_PARAMS), { wrapper: makeWrapper(queryClient) });

    // t=5s: job2 finishes.
    const job2Done = makeJob({ id: 2, bytesDone: 49_000_000, bytesTotal: 49_000_000, state: 'IMPORTING' });
    queryClient.setQueryData(queryKeys.live, { jobs: [job2Done], down: 0, up: 0 });
    rerender();
    expect(result.current.data?.jobs[1]).toMatchObject({ state: 'IMPORTING', bytesDone: 49_000_000 });

    // t=6s: only job1 changed. The accumulated cache (what stream.tsx would
    // actually write) still carries job2Done alongside job1's update — job2
    // must not revert to its 44MB/DOWNLOADING REST snapshot just because
    // this tick's delta didn't mention it.
    const job1Changed = makeJob({ id: 1, bytesDone: 20, bytesTotal: 100 });
    queryClient.setQueryData(queryKeys.live, { jobs: [job1Changed, job2Done], down: 0, up: 0 });
    rerender();
    expect(result.current.data?.jobs[0]).toMatchObject({ bytesDone: 20 });
    expect(result.current.data?.jobs[1]).toMatchObject({ state: 'IMPORTING', bytesDone: 49_000_000 });
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
    queryClient.setQueryData(queryKeys.live, { jobs: [], down: 0, up: 0, scopeJobId: 1, detail: streamed });
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
