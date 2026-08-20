import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_JOB_PAGE_PARAMS,
  isTerminalJobFilter,
  jobsPageUrl,
  replaceLiveJobPage,
  pickJobDetail,
  replaceLiveJobs,
  mergeThroughputSamples,
  queryKeys,
  useJobDetail,
  useJobs,
} from './queries';
import type { Job, JobDetail, JobPage, JobStatusFilter, ScopedLivePayload, ThroughputSample, WireJob } from './types';

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
    // Computed at call time (not a fixed literal) so it reads as "just
    // framed" under Date.now() whenever the test actually runs, and every
    // existing call site that doesn't care about live-data freshness keeps
    // passing untouched. Tests that specifically exercise freshness (see
    // the 'replaceLiveJobs' describe block) override this explicitly.
    framedAt: new Date().toISOString(),
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
        lastResort: false,
      },
    ],
    ...overrides,
  };
}

describe('replaceLiveJobs', () => {
  // Fixed clock for the freshness-boundary tests below — see LIVE_JOB_FRESH_MS
  // in queries.ts (not exported; mirrored here as FRESH_MS so a drift between
  // the two is visible rather than silently tolerated).
  const NOW = new Date('2026-07-27T10:00:00Z');
  const FRESH_MS = 10_000; // must match queries.ts's LIVE_JOB_FRESH_MS

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });
  afterEach(() => vi.useRealTimers());

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
  // correct a job pinned at stale live values. `framedAt` is: once the
  // streamed row's framedAt ages past LIVE_JOB_FRESH_MS, REST wins and the
  // pin is released — regardless of what either side's updatedAt says,
  // since the two are read from independently-cached DB copies and can't be
  // compared meaningfully (issue #285).
  it('lets REST win once the pinned streamed row goes stale', () => {
    const stale = new Date(NOW.getTime() - FRESH_MS - 1).toISOString();
    const pinned = makeJob({ id: 1, state: 'DOWNLOADING', status: 'active', speed: 0, framedAt: stale });
    const restAfterReconcile = makeJob({ id: 1, state: 'DONE', status: 'done', framedAt: NOW.toISOString() });

    const replaced = replaceLiveJobs([restAfterReconcile], [pinned]);
    expect(replaced?.[0]).toEqual(restAfterReconcile);

    // A later read with the same stale pinned row must keep losing — the
    // pin was released, not just skipped once.
    const replacedAgain = replaceLiveJobs([restAfterReconcile], [pinned]);
    expect(replacedAgain?.[0]).toEqual(restAfterReconcile);
  });

  // The mirror image: a recently framed streamed row wins over REST even
  // when the two disagree on state — this is the ordinary, expected case
  // the accumulator exists for.
  it('prefers the live object while its framedAt is still fresh', () => {
    const jobs = [makeJob({ id: 1, state: 'DOWNLOADING', bytesDone: 50, bytesTotal: 100 })];
    const streamed = makeJob({ id: 1, state: 'DOWNLOADING', bytesDone: 80, bytesTotal: 100, speed: 4000, framedAt: NOW.toISOString() });
    expect(replaceLiveJobs(jobs, [streamed])?.[0]).toEqual(streamed);
  });

  // Boundary: exactly FRESH_MS old is still fresh (the check is a strict
  // `>`, so equality does not count as stale).
  it('still trusts a streamed row exactly LIVE_JOB_FRESH_MS old', () => {
    const jobs = [makeJob({ id: 1 })];
    const boundary = new Date(NOW.getTime() - FRESH_MS).toISOString();
    const streamed = makeJob({ id: 1, speed: 4000, framedAt: boundary });
    expect(replaceLiveJobs(jobs, [streamed])?.[0]).toEqual(streamed);
  });

  // One tick past the boundary flips to stale.
  it('stops trusting a streamed row one millisecond past LIVE_JOB_FRESH_MS', () => {
    const jobs = [makeJob({ id: 1 })];
    const pastBoundary = new Date(NOW.getTime() - FRESH_MS - 1).toISOString();
    const streamed = makeJob({ id: 1, speed: 4000, framedAt: pastBoundary });
    expect(replaceLiveJobs(jobs, [streamed])?.[0]).toEqual(jobs[0]);
  });
});

describe('paged jobs transport', () => {
  it('isolates every request primitive in query keys and URL-encodes search values', () => {
    const params = { ...DEFAULT_JOB_PAGE_PARAMS, page: 2, sort: 'peer' as const, dir: 'desc' as const, filter: 'failed' as const, source: 'manual' as const, q: 'Miles & Blue' };
    const key = queryKeys.jobsPage(params);

    expect(key).toEqual(['jobs', 'page', 2, 'peer', 'desc', 'failed', 'manual', 'Miles & Blue', undefined, undefined]);
    expect(queryKeys.jobsPage({ ...params, source: 'lidarr' })).not.toEqual(key);
    expect(jobsPageUrl(params)).toBe('/api/jobs?page=2&sort=peer&dir=desc&filter=failed&source=manual&q=Miles+%26+Blue');
  });

  // Overview (issue #268) requests a smaller page than the paged Jobs route,
  // so pageSize has to isolate its own cache entry and reach the URL.
  it('sends pageSize only when given, and isolates it in the query key', () => {
    const withSize = { ...DEFAULT_JOB_PAGE_PARAMS, pageSize: 8 };
    expect(queryKeys.jobsPage(withSize)).toEqual(['jobs', 'page', 0, 'st', 'asc', 'all', 'all', '', 8, undefined]);
    expect(queryKeys.jobsPage(withSize)).not.toEqual(queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS));
    expect(jobsPageUrl(withSize)).toBe('/api/jobs?page=0&sort=st&dir=asc&filter=all&source=all&q=&pageSize=8');
    expect(jobsPageUrl(DEFAULT_JOB_PAGE_PARAMS)).not.toContain('pageSize');
  });

  it('overlays current-page IDs only while preserving order and metadata', () => {
    const page: JobPage = {
      jobs: [makeJob({ id: 2 }), makeJob({ id: 1 })],
      total: 25,
      facets: {
        status: { all: 25, wanted: 0, selecting: 0, queued: 0, active: 2, importing: 1, waiting: 3, stalled: 4, failed: 5, parked: 6, done: 4, notImported: 0, importRefused: 0 },
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
        status: { all: 1, wanted: 0, selecting: 0, queued: 0, active: 1, importing: 0, waiting: 0, stalled: 0, failed: 0, parked: 0, done: 0, notImported: 0, importRefused: 0 },
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
        status: { all: 2, wanted: 0, selecting: 0, queued: 0, active: 2, importing: 0, waiting: 0, stalled: 0, failed: 0, parked: 0, done: 0, notImported: 0, importRefused: 0 },
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

describe('isTerminalJobFilter', () => {
  // Declared as an exhaustive Record over the union rather than a hand-written
  // array of the terminal ones: adding a member to JobStatusFilter without
  // classifying it here is a type error, so a new filter cannot quietly
  // inherit the live merge (or quietly lose it).
  const EXPECTED: Record<JobStatusFilter, boolean> = {
    all: false,
    wanted: false,
    selecting: false,
    queued: false,
    waiting: false,
    active: false,
    importing: false,
    stalled: false,
    // Status-derived, and deliberately NOT terminal: it also matches a job
    // still DOWNLOADING whose current candidate's transfers all errored and
    // which the pipeline will retry (see dashboardJobsWhere's default case).
    // That job is genuinely live and must keep its streamed row.
    failed: false,
    parked: true,
    done: true,
    notImported: true,
    importRefused: true,
    inflight: false,
    finished: true,
    failures: true,
  };

  it('classifies every job filter', () => {
    for (const [filter, want] of Object.entries(EXPECTED)) {
      expect(isTerminalJobFilter(filter as JobStatusFilter)).toBe(want);
    }
  });
});

function makePage(jobs: Job[]): JobPage {
  return {
    jobs,
    total: jobs.length,
    facets: {
      status: { all: jobs.length, wanted: 0, selecting: 0, queued: 0, active: 0, importing: 0, waiting: 0, stalled: 0, failed: 0, parked: 0, done: 0, notImported: 0, importRefused: 0 },
      source: { all: jobs.length, manual: 0, lidarr: jobs.length },
    },
  };
}

describe('useJobs on a terminal filter', () => {
  // Issue #291, symptom 2: the live payload accumulated by stream.tsx used to
  // be merged into every page useJobs returned, including RECENTLY FINISHED's.
  // A row whose framedAt was still fresh therefore won over the settled REST
  // row and rendered an ACTIVE/DL tag — plus its pre-completion updatedAt in
  // the WHEN column — inside a panel of terminal jobs. This is the case no
  // browser session ever managed to catch inside the freshness window.
  it('ignores a fresh streamed row on a finished page', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const params = { ...DEFAULT_JOB_PAGE_PARAMS, filter: 'finished' as const, sort: 'recent' as const, dir: 'desc' as const, skipFacets: true };
    const settled = makeJob({ id: 1, state: 'DONE', status: 'done', updatedAt: '2026-07-01T10:05:00Z' });
    queryClient.setQueryData(queryKeys.jobsPage(params), makePage([settled]));

    const { result, rerender } = renderHook(() => useJobs(params), { wrapper: makeWrapper(queryClient) });

    // framedAt defaults to "just now", i.e. comfortably inside
    // LIVE_JOB_FRESH_MS — exactly the row that used to win here.
    queryClient.setQueryData(queryKeys.live, {
      jobs: [makeJob({ id: 1, state: 'DOWNLOADING', status: 'active', speed: 4000, updatedAt: '2026-07-01T10:00:00Z' })],
      down: 4000,
      up: 0,
    });
    rerender();

    expect(result.current.data?.jobs[0]).toEqual(settled);
    expect(result.current.data?.jobs[0].status).toBe('done');
    expect(result.current.data?.jobs[0].updatedAt).toBe('2026-07-01T10:05:00Z');
  });

  it('still merges live data into the in-flight page', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const params = { ...DEFAULT_JOB_PAGE_PARAMS, filter: 'inflight' as const, sort: 'transfer' as const };
    queryClient.setQueryData(queryKeys.jobsPage(params), makePage([makeJob({ id: 1, bytesDone: 50, bytesTotal: 100 })]));

    const { result, rerender } = renderHook(() => useJobs(params), { wrapper: makeWrapper(queryClient) });

    queryClient.setQueryData(queryKeys.live, {
      jobs: [makeJob({ id: 1, bytesDone: 80, bytesTotal: 100, speed: 4000 })],
      down: 4000,
      up: 0,
    });
    rerender();

    expect(result.current.data?.jobs[0]).toMatchObject({ bytesDone: 80, speed: 4000 });
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

// Issue #274: the fetch half of the same predicate. pickJobDetail's four
// branches are unit-tested above; these two pin down that refetchInterval
// follows them rather than polling unconditionally underneath.
describe('useJobDetail poll gating', () => {
  afterEach(() => vi.useRealTimers());

  // The detail body served here is irrelevant to what is being measured — only
  // the call count is — but it has to resolve, because React Query never
  // starts an interval refetch while a fetch is still in flight.
  function stubDetailFetch() {
    const fetchMock = vi.fn(() => Promise.resolve({ ok: true, json: () => Promise.resolve(makeDetail()) } as Response));
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
  }

  it('stops polling while the stream carries this job\'s detail, and resumes when it drops', async () => {
    vi.useFakeTimers();
    const fetchMock = stubDetailFetch();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    const { rerender } = renderHook(() => useJobDetail(1), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    // The mount GET always happens: REST is still the source of truth for the
    // snapshot, and the stream has said nothing yet.
    expect(fetchMock).toHaveBeenCalledTimes(1);

    act(() => {
      queryClient.setQueryData(queryKeys.live, { jobs: [], down: 0, up: 0, scopeJobId: 1, detail: makeDetail() });
    });
    rerender();
    // Three whole JOB_DETAIL_INTERVALs with a live scoped stream: not one
    // further GET. This is the issue's own acceptance check ("count GET
    // /api/jobs/{id}/detail on an open /jobs/:id: zero after the first").
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Stream drops — StreamProvider's clearLive writes null — and the poll is
    // the fallback again.
    act(() => { queryClient.setQueryData(queryKeys.live, null); });
    rerender();
    await act(async () => { await vi.advanceTimersByTimeAsync(3_500); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });

  // The trap the issue calls out: a live stream scoped to *other* jobs (the
  // /jobs list's ?jobs=<ids> connection, under an expanded JobExpansion row)
  // carries no detail for this one. Gating on "is the stream up?" instead of
  // "is it scoped to this job?" would freeze the panel — polling off, nothing
  // arriving to replace it.
  it('keeps polling when the live frame is scoped to a different job', async () => {
    vi.useFakeTimers();
    const fetchMock = stubDetailFetch();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(queryKeys.live, { jobs: [makeJob({ id: 2 })], down: 0, up: 0, scopeJobId: 2, detail: makeDetail() });

    renderHook(() => useJobDetail(1), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await act(async () => { await vi.advanceTimersByTimeAsync(3_500); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });

  // The scope alone is not enough to hand ownership over. A ?job=<id>
  // connection can be open and framing while the hub still has no detail body
  // to send for it — buildStreamDetail returns nil until both the cached
  // detail and the job view are populated (internal/observ/stream.go). Gating
  // on scope without checking for a body would stop the poll in exactly that
  // window, leaving the view with no writer at all.
  it('keeps polling when a frame scoped to this job carries no detail', async () => {
    vi.useFakeTimers();
    const fetchMock = stubDetailFetch();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(queryKeys.live, { jobs: [], down: 0, up: 0, scopeJobId: 1 });

    renderHook(() => useJobDetail(1), { wrapper: makeWrapper(queryClient) });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await act(async () => { await vi.advanceTimersByTimeAsync(3_500); });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });
});
