import { useEffect, useState } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen } from '@testing-library/react';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DEFAULT_JOB_PAGE_PARAMS, queryKeys } from './queries';
import { JOBS_CACHE_LIMIT, StreamProvider, useJobScope, useSearchStream, useThroughputStream } from './stream';
import type { SearchSession } from './types';

// EventSource does not exist in jsdom — this mock captures every instance
// ever constructed (StreamProvider.tsx creates a new one per route change)
// and lets a test drive onopen/onerror and dispatch named `live` events
// without a real network connection.
class MockEventSource {
  static instances: MockEventSource[] = [];
  url: string;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  private listeners = new Map<string, ((event: MessageEvent) => void)[]>();

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }

  addEventListener(type: string, handler: (event: MessageEvent) => void) {
    const list = this.listeners.get(type) ?? [];
    list.push(handler);
    this.listeners.set(type, list);
  }

  emit(type: string, data: unknown) {
    for (const handler of this.listeners.get(type) ?? []) {
      handler({ data: JSON.stringify(data) } as MessageEvent);
    }
  }

  close() {
    this.closed = true;
  }
}

beforeEach(() => {
  MockEventSource.instances = [];
  vi.stubGlobal('EventSource', MockEventSource);
});

afterEach(() => vi.unstubAllGlobals());

function NavigateOnClick({ to }: { to: string }) {
  const navigate = useNavigate();
  return <button onClick={() => navigate(to)}>go</button>;
}

// Mirrors how Jobs.tsx calls useJobScope: publishes `ids` for as long as it's
// mounted with them.
function ScopePublisher({ ids }: { ids: number[] | undefined }) {
  useJobScope(ids);
  return null;
}

// A fresh array with the same content on every render — exactly what Jobs.tsx
// produces on each JOBS_INTERVAL poll/rerender even when the page's ids
// haven't actually changed.
function SameContentScope() {
  const [, forceRerender] = useState(0);
  return (
    <>
      <ScopePublisher ids={[1, 2, 3]} />
      <button onClick={() => forceRerender((n) => n + 1)}>rerender</button>
    </>
  );
}

// Toggles between two genuinely different id sets on each click.
function TogglingScope() {
  const [alt, setAlt] = useState(false);
  return (
    <>
      <ScopePublisher ids={alt ? [4, 5] : [1, 2, 3]} />
      <button onClick={() => setAlt((a) => !a)}>toggle</button>
    </>
  );
}

// Mirrors how Search.tsx scopes the connection: each click publishes a new
// session id, which reopens the connection exactly as starting a new search
// does.
function SearchPublisher({ id }: { id: string }) {
  useSearchStream(id);
  return null;
}

function TogglingSearch() {
  const [n, setN] = useState(0);
  return (
    <>
      <SearchPublisher id={`session-${n}`} />
      <button onClick={() => setN((v) => v + 1)}>next search</button>
    </>
  );
}

describe('StreamProvider', () => {
  it('opens the unscoped endpoint outside a job route', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/stream');
  });

  it('opens ?job=<id> on a job detail route and reopens on a fresh connection when the id changes', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs/1']}>
          <StreamProvider>{null}</StreamProvider>
          <NavigateOnClick to="/jobs/2" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/stream?job=1');

    act(() => document.querySelector('button')!.click());

    expect(MockEventSource.instances).toHaveLength(2);
    expect(MockEventSource.instances[0].closed).toBe(true);
    expect(MockEventSource.instances[1].url).toBe('/api/stream?job=2');
    expect(MockEventSource.instances[1].closed).toBe(false);
  });

  // useJobScope publishes into StreamProvider's own state, so mounting a
  // publisher reopens the connection once with `?jobs=...` — the initial
  // unscoped instance is a one-render artifact, closed as soon as the
  // publisher's effect runs (issue #258).
  it('opens ?jobs=<ids> once a route publishes a job scope', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <SameContentScope />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(2);
    expect(MockEventSource.instances[0].closed).toBe(true);
    expect(MockEventSource.instances[1].url).toBe('/api/stream?jobs=1,2,3');
    expect(MockEventSource.instances[1].closed).toBe(false);
  });

  // The ids array is rebuilt fresh on every render (exactly what Jobs.tsx
  // does on its JOBS_INTERVAL poll), so useJobScope must dedupe on the
  // joined ids string, not on array identity — otherwise every poll would
  // reopen the connection in a loop.
  it('does not reopen the connection when the same ids arrive as a new array instance', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <SameContentScope />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(2);

    act(() => document.querySelector('button')!.click());

    expect(MockEventSource.instances).toHaveLength(2);
    expect(MockEventSource.instances[1].closed).toBe(false);
  });

  it('reopens the connection when the published scope changes to different ids', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <TogglingScope />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(2);
    expect(MockEventSource.instances[1].url).toBe('/api/stream?jobs=1,2,3');

    act(() => document.querySelector('button')!.click());

    expect(MockEventSource.instances).toHaveLength(3);
    expect(MockEventSource.instances[1].closed).toBe(true);
    expect(MockEventSource.instances[2].url).toBe('/api/stream?jobs=4,5');
    expect(MockEventSource.instances[2].closed).toBe(false);
  });

  it('invalidates the REST queries on open, including job detail when scoped to a job', () => {
    const queryClient = new QueryClient();
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries');
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs/1']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].onopen?.());

    const invalidatedKeys = invalidateSpy.mock.calls.map((c) => c[0]?.queryKey);
    expect(invalidatedKeys).toContainEqual(queryKeys.jobs);
    expect(invalidatedKeys).toContainEqual(queryKeys.charts);
    expect(invalidatedKeys).toContainEqual(queryKeys.jobDetail(1));
  });

  // The jobs/charts invalidation is a gap repair, so it belongs only to the
  // two opens that can have a gap behind them: the first ever, and the
  // browser's own reconnect (which reuses the same EventSource and fires
  // onopen again on it). Issue #58 made the third kind common — the
  // `?search=<id>` axis reopens on every new search, and each of those used
  // to cost a GET /api/charts (TopBar mounts useCharts on every route, so
  // charts is always an active query) for a view that renders no chart.
  it('does not re-invalidate jobs and charts when a new search merely reopens the connection', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/search']}>
          <StreamProvider>
            <TogglingSearch />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    // First connection open: this one does invalidate.
    act(() => MockEventSource.instances.at(-1)!.onopen?.());

    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries');
    const before = MockEventSource.instances.length;
    act(() => screen.getByText('next search').click());
    expect(MockEventSource.instances.length).toBeGreaterThan(before);

    act(() => MockEventSource.instances.at(-1)!.onopen?.());

    const keys = invalidateSpy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).not.toContainEqual(queryKeys.jobs);
    expect(keys).not.toContainEqual(queryKeys.charts);
  });

  // The other half of the same contract: an actual drop-and-reconnect fires
  // onopen a second time on the SAME instance, and that one must still take a
  // fresh snapshot — it is the case the invalidation exists for.
  it('invalidates jobs and charts again when the browser reconnects the same connection', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/search']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    const source = MockEventSource.instances[0];
    act(() => source.onopen?.());

    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries');
    act(() => source.onerror?.());
    act(() => source.onopen?.());

    const keys = invalidateSpy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(queryKeys.jobs);
    expect(keys).toContainEqual(queryKeys.charts);
  });

  it('writes a `live` frame into the live cache, scoped to the current job id', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs/7']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [{ id: 7, bytesDone: 1, bytesTotal: 2 }],
      down: 500,
      up: 250,
    }));

    expect(queryClient.getQueryData(queryKeys.live)).toMatchObject({
      down: 500,
      up: 250,
      scopeJobId: 7,
      jobs: [{ id: 7, bytesDone: 1, bytesTotal: 2 }],
    });
  });

  // Issue #258's fix-round bug: `jobs` on the wire is a per-tick delta of
  // changed jobs, not a snapshot of the live set. A frame that carries job1
  // but not job2 means "job2 didn't change this tick", not "job2 has no live
  // data" — so the cache must keep accumulating job2's last-known values
  // rather than losing them the moment a frame doesn't mention it.
  it('accumulates jobs across frames by id, keeping a job that drops out of a later frame', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [{ id: 2, bytesDone: 49_000_000, bytesTotal: 49_000_000, state: 'IMPORTING' }],
      down: 0,
      up: 0,
    }));
    expect(queryClient.getQueryData(queryKeys.live)).toMatchObject({
      jobs: [{ id: 2, bytesDone: 49_000_000, state: 'IMPORTING' }],
    });

    // Next tick: only job1 changed. job2 is absent from this frame but must
    // still be present in the cache, unchanged from the previous frame.
    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [{ id: 1, bytesDone: 20, bytesTotal: 100 }],
      down: 0,
      up: 0,
    }));
    const live = queryClient.getQueryData(queryKeys.live) as { jobs: { id: number }[] };
    expect(live.jobs).toHaveLength(2);
    expect(live.jobs).toContainEqual(expect.objectContaining({ id: 1, bytesDone: 20 }));
    expect(live.jobs).toContainEqual(expect.objectContaining({ id: 2, bytesDone: 49_000_000, state: 'IMPORTING' }));
  });

  // D1: an unscoped connection (Overview, JobDetail — neither calls
  // useJobScope) never has its job ids narrowed, so without a cap the
  // accumulator would grow for the connection's entire lifetime — every job
  // that ever went live, never evicted, since the backend simply stops
  // streaming a job once it leaves the live-matched set. Eviction is safe
  // by construction (falling back to REST is always correct), and
  // delete-then-set on every update means the least-recently-updated entry
  // is always the one dropped, never an actively-changing job.
  it('evicts the least-recently-updated job once the accumulator exceeds its cap', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Fill the accumulator to its cap with ids 1..JOBS_CACHE_LIMIT.
    act(() => MockEventSource.instances[0].emit('live', {
      jobs: Array.from({ length: JOBS_CACHE_LIMIT }, (_, i) => ({ id: i + 1, bytesDone: 1, bytesTotal: 2 })),
      down: 0,
      up: 0,
    }));
    let live = queryClient.getQueryData(queryKeys.live) as { jobs: { id: number }[] };
    expect(live.jobs).toHaveLength(JOBS_CACHE_LIMIT);
    expect(live.jobs.some((j) => j.id === 1)).toBe(true);

    // One more, previously untracked id pushes it over the cap. Job 1 is the
    // oldest untouched entry, so it's the one evicted; the freshly-touched
    // job 2 (re-updated in this same frame) must not be.
    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [
        { id: 2, bytesDone: 5, bytesTotal: 10 },
        { id: JOBS_CACHE_LIMIT + 1, bytesDone: 1, bytesTotal: 2 },
      ],
      down: 0,
      up: 0,
    }));
    live = queryClient.getQueryData(queryKeys.live) as { jobs: { id: number }[] };
    expect(live.jobs).toHaveLength(JOBS_CACHE_LIMIT);
    expect(live.jobs.some((j) => j.id === 1)).toBe(false);
    expect(live.jobs.some((j) => j.id === 2)).toBe(true);
    expect(live.jobs.some((j) => j.id === JOBS_CACHE_LIMIT + 1)).toBe(true);
  });

  // clearLive resets the accumulator, which is correct because every
  // reconnect is preceded by a fresh GET (onopen's invalidation) — so a job
  // accumulated on a since-dropped connection must not leak into the next one.
  it('resets the accumulated jobs when the connection errors', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [{ id: 2, bytesDone: 49_000_000, bytesTotal: 49_000_000, state: 'IMPORTING' }],
      down: 0,
      up: 0,
    }));
    act(() => MockEventSource.instances[0].onerror?.());
    expect(queryClient.getQueryData(queryKeys.live)).toBeNull();

    // A frame arriving right after (the browser's own automatic reconnect,
    // same effect instance) starts the accumulation over — job2 is gone.
    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [{ id: 1, bytesDone: 20, bytesTotal: 100 }],
      down: 0,
      up: 0,
    }));
    const live = queryClient.getQueryData(queryKeys.live) as { jobs: { id: number }[] };
    expect(live.jobs).toEqual([{ id: 1, bytesDone: 20, bytesTotal: 100 }]);
  });

  // Contrast with the reconnect case below (issue #276): a scope change must
  // NOT clear, but a genuine stream failure still has to — and since the
  // cleanup no longer clears, this is the only path that does mid-session.
  it('clears the live cache on error, so consumers fall back to REST', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('live', { jobs: [], down: 1000, up: 500 }));
    expect(queryClient.getQueryData(queryKeys.live)).toBeDefined();

    // null, not undefined: setQueryData ignores an undefined value, so
    // clearing has to write a real sentinel to notify subscribers. Asserting
    // toBeUndefined() here would pass against a no-op clear that silently
    // left the last frame overlaid — which is the bug this test guards.
    act(() => MockEventSource.instances[0].onerror?.());
    expect(queryClient.getQueryData(queryKeys.live)).toBeNull();
  });

  // Issue #276: Overview/Jobs republish their scope on every JOBS_INTERVAL
  // poll as job state churns (see useJobScope), which reopens the connection
  // via this exact dep-array change. That must not blank every row on screen
  // for the gap until the new connection's first frame lands.
  it('keeps accumulated live data across a scope change, before any new frame arrives', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <TogglingScope />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(2);

    act(() => MockEventSource.instances[1].emit('live', {
      jobs: [{ id: 1, bytesDone: 10, bytesTotal: 100 }],
      down: 0,
      up: 0,
    }));
    expect(queryClient.getQueryData(queryKeys.live)).toMatchObject({
      jobs: [{ id: 1, bytesDone: 10, bytesTotal: 100 }],
    });

    // Reopens the connection (different published ids) — the old data must
    // survive until a new frame actually arrives.
    act(() => document.querySelector('button')!.click());
    expect(MockEventSource.instances).toHaveLength(3);
    expect(MockEventSource.instances[1].closed).toBe(true);
    expect(queryClient.getQueryData(queryKeys.live)).toMatchObject({
      jobs: [{ id: 1, bytesDone: 10, bytesTotal: 100 }],
    });
  });

  // The other half of the reconnect-vs-unmount distinction: a real unmount
  // (leaving the app / tearing down StreamProvider) must still clear, or a
  // stale live overlay would linger in the cache forever — the exact
  // failure the REST fallback exists to prevent.
  it('clears the live cache when StreamProvider genuinely unmounts', () => {
    const queryClient = new QueryClient();
    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('live', {
      jobs: [{ id: 1, bytesDone: 10, bytesTotal: 100 }],
      down: 0,
      up: 0,
    }));
    expect(queryClient.getQueryData(queryKeys.live)).not.toBeNull();

    act(() => unmount());
    expect(queryClient.getQueryData(queryKeys.live)).toBeNull();
  });

  // Issue #265: throughput arrives on its own `event: throughput`, decoupled
  // from `event: live` entirely.
  it('merges and dedupes download and upload samples independently', () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: [{ at: '2026-07-26T12:00:00Z', bytesPerSecond: 1000, activeTransfers: 1 }],
      uploadThroughput: [{ at: '2026-07-26T12:00:00Z', bytesPerSecond: 500, activeTransfers: 1 }],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() =>
      MockEventSource.instances[0].emit('throughput', {
        download: [
          { at: '2026-07-26T12:00:00Z', bytesPerSecond: 1000, activeTransfers: 1 },
          { at: '2026-07-26T12:00:01Z', bytesPerSecond: 2000, activeTransfers: 2 },
        ],
        upload: [
          { at: '2026-07-26T12:00:01Z', bytesPerSecond: 750, activeTransfers: 1 },
          { at: '2026-07-26T12:00:02Z', bytesPerSecond: 0, activeTransfers: 0 },
        ],
      }),
    );

    const charts = queryClient.getQueryData(queryKeys.charts) as {
      throughput: { at: string }[];
      uploadThroughput: { at: string }[];
    };
    expect(charts.throughput.map((sample) => sample.at)).toEqual([
      '2026-07-26T12:00:00Z',
      '2026-07-26T12:00:01Z',
    ]);
    expect(charts.uploadThroughput.map((sample) => sample.at)).toEqual([
      '2026-07-26T12:00:00Z',
      '2026-07-26T12:00:01Z',
      '2026-07-26T12:00:02Z',
    ]);
  });

  it('caps each cached direction at 48 samples without changing the other', () => {
    const queryClient = new QueryClient();
    const samples = Array.from({ length: 48 }, (_, index) => ({
      at: `2026-07-26T12:00:${String(index).padStart(2, '0')}Z`,
      bytesPerSecond: index,
      activeTransfers: 1,
    }));
    queryClient.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: samples,
      uploadThroughput: samples,
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('throughput', {
      upload: [
        { at: '2026-07-26T12:01:00Z', bytesPerSecond: 999, activeTransfers: 1 },
      ],
    }));

    const charts = queryClient.getQueryData(queryKeys.charts) as {
      throughput: { at: string }[];
      uploadThroughput: { at: string }[];
    };
    expect(charts.throughput).toHaveLength(48);
    expect(charts.throughput[0].at).toBe('2026-07-26T12:00:00Z');
    expect(charts.uploadThroughput).toHaveLength(48);
    expect(charts.uploadThroughput[0].at).toBe('2026-07-26T12:00:01Z');
    expect(charts.uploadThroughput.at(-1)?.at).toBe('2026-07-26T12:01:00Z');
  });

  // Proves the two events are genuinely decoupled: a `throughput` frame
  // merges into the charts cache even with no preceding `live` frame at all
  // on this connection.
  it('merges a throughput event into the charts cache with no preceding live frame', () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: [],
      uploadThroughput: [],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('throughput', {
      download: [{ at: '2026-07-26T12:00:00Z', bytesPerSecond: 1000, activeTransfers: 1 }],
    }));

    const charts = queryClient.getQueryData(queryKeys.charts) as { throughput: { at: string }[] };
    expect(charts.throughput).toEqual([
      { at: '2026-07-26T12:00:00Z', bytesPerSecond: 1000, activeTransfers: 1 },
    ]);
  });

  // TestInvalidatePayloadExactBidirectionalJSON's Go-side counterpart: proves
  // the client's reaction to `event: invalidate` (issue #275) is scoped to
  // page queries only, per decision 6 — a bare `invalidateQueries({
  // queryKey: queryKeys.jobs })` would also match jobDetail(id) and
  // jobEvents(id), forcing a detail refetch that #274 deliberately removed.
  //
  // Also covers the #371 fix directly: a jobs-only frame (`uploads: false`)
  // must NOT invalidate queryKeys.uploadHistory — the defect this change
  // exists to close (see the `invalidate` listener's doc comment in
  // stream.tsx and useUploadHistory's in queries.ts for why that query's own
  // design depends on it).
  it('refetches the jobs page, and only the jobs page, on a jobs-only invalidate event', () => {
    const queryClient = new QueryClient();
    const pageKey = queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS);
    const detailKey = queryKeys.jobDetail(1);
    queryClient.setQueryData(pageKey, { jobs: [], total: 0, facets: null });
    queryClient.setQueryData(detailKey, { job: { id: 1 }, attempts: [] });
    queryClient.setQueryData(queryKeys.uploadHistory, { pages: [{ uploads: [], hasMore: false }], pageParams: [0] });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs/1']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    // onopen's own invalidation already ran on mount (it invalidates
    // queryKeys.jobs unconditionally) — reset every query's invalidated
    // state so this test only observes the `invalidate` listener's own
    // effect.
    void queryClient.getQueryCache().find({ queryKey: pageKey })?.setState({ isInvalidated: false });
    void queryClient.getQueryCache().find({ queryKey: detailKey })?.setState({ isInvalidated: false });
    void queryClient.getQueryCache().find({ queryKey: queryKeys.uploadHistory })?.setState({ isInvalidated: false });

    act(() => MockEventSource.instances[0].emit('invalidate', { generation: 1, jobs: true, uploads: false }));

    expect(queryClient.getQueryState(pageKey)?.isInvalidated).toBe(true);
    // Decision 6's whole point: a mounted jobDetail query must NOT be
    // invalidated by this event — jobDetail(id) also shares the `jobs`
    // prefix, and #274 deliberately turned off its own poll because the
    // stream's `detail` field already keeps it live.
    expect(queryClient.getQueryState(detailKey)?.isInvalidated).toBe(false);
    // The #371 regression guard: `uploads: false` on this frame must leave
    // upload history untouched.
    expect(queryClient.getQueryState(queryKeys.uploadHistory)?.isInvalidated).toBe(false);
  });

  // internal/observ TestStreamHubUploadMarkAloneBumpsGeneration's client-side
  // counterpart (issue #366): an uploads-only invalidate event must refetch
  // upload history. Also covers the #371 fix's mirror image: it must NOT
  // invalidate the jobs page (`jobs: false`) — a finished upload has nothing
  // to do with GET /api/jobs.
  it('refetches upload history, and only upload history, on an uploads-only invalidate event', () => {
    const queryClient = new QueryClient();
    const pageKey = queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS);
    queryClient.setQueryData(pageKey, { jobs: [], total: 0, facets: null });
    queryClient.setQueryData(queryKeys.uploadHistory, { pages: [{ uploads: [], hasMore: false }], pageParams: [0] });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    void queryClient.getQueryCache().find({ queryKey: pageKey })?.setState({ isInvalidated: false });
    void queryClient.getQueryCache().find({ queryKey: queryKeys.uploadHistory })?.setState({ isInvalidated: false });

    act(() => MockEventSource.instances[0].emit('invalidate', { generation: 1, jobs: false, uploads: true }));

    expect(queryClient.getQueryState(queryKeys.uploadHistory)?.isInvalidated).toBe(true);
    // The #371 regression guard: `jobs: false` on this frame must leave the
    // jobs page untouched — this is exactly the busy-system cost this fix
    // removes (an upload finishing must not re-fetch every loaded page of
    // upload history's OWN infinite query either, but that is covered by
    // TanStack's own invalidateQueries semantics once the jobs half is no
    // longer wrongly folded in).
    expect(queryClient.getQueryState(pageKey)?.isInvalidated).toBe(false);
  });

  // Helper to stub document.hidden for the visibility-gated invalidate tests
  // below. jsdom's `document.hidden` is a getter with no setter by default,
  // so it must be redefined via Object.defineProperty; configurable: true
  // lets afterEach restore it so other tests in this file (and other files
  // sharing jsdom's single `document`) never see a stuck value.
  function setDocumentHidden(hidden: boolean) {
    Object.defineProperty(document, 'hidden', { value: hidden, configurable: true });
  }

  afterEach(() => {
    setDocumentHidden(false);
  });

  // A background tab must cost nothing (issue #275 review, decision on FIX
  // 2): invalidateQueries refetches an active query regardless of tab
  // visibility, unlike refetchInterval (refetchIntervalInBackground defaults
  // false), and the SSE connection stays open while backgrounded — so
  // without this guard a parked tab would go from zero requests to up to 4/
  // minute, worse than the poll this feature replaced.
  it('does not invalidate the jobs page on an invalidate event while the tab is hidden', () => {
    const queryClient = new QueryClient();
    const pageKey = queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS);
    queryClient.setQueryData(pageKey, { jobs: [], total: 0, facets: null });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    void queryClient.getQueryCache().find({ queryKey: pageKey })?.setState({ isInvalidated: false });

    setDocumentHidden(true);
    act(() => MockEventSource.instances[0].emit('invalidate', { generation: 1, jobs: true, uploads: false }));

    expect(queryClient.getQueryState(pageKey)?.isInvalidated).toBe(false);
  });

  // The same guard covers upload history (issue #366): a parked tab must not
  // pick up 4 refetches/minute for a view it likely doesn't even have open.
  it('does not invalidate upload history on an invalidate event while the tab is hidden', () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(queryKeys.uploadHistory, { pages: [{ uploads: [], hasMore: false }], pageParams: [0] });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    void queryClient.getQueryCache().find({ queryKey: queryKeys.uploadHistory })?.setState({ isInvalidated: false });

    setDocumentHidden(true);
    act(() => MockEventSource.instances[0].emit('invalidate', { generation: 1, jobs: false, uploads: true }));

    expect(queryClient.getQueryState(queryKeys.uploadHistory)?.isInvalidated).toBe(false);
  });

  // The other half of the same guard: a visible tab must still invalidate
  // exactly as before — the hidden check must not accidentally swallow the
  // common case.
  it('still invalidates the jobs page on an invalidate event while the tab is visible', () => {
    const queryClient = new QueryClient();
    const pageKey = queryKeys.jobsPage(DEFAULT_JOB_PAGE_PARAMS);
    queryClient.setQueryData(pageKey, { jobs: [], total: 0, facets: null });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    void queryClient.getQueryCache().find({ queryKey: pageKey })?.setState({ isInvalidated: false });

    setDocumentHidden(false);
    act(() => MockEventSource.instances[0].emit('invalidate', { generation: 1, jobs: true, uploads: false }));

    expect(queryClient.getQueryState(pageKey)?.isInvalidated).toBe(true);
  });

  // The negative half: nothing about the `live` listener (or any other
  // event) should trigger the jobs-page invalidation — only `event:
  // invalidate` does.
  it('does not invalidate the jobs page before an invalidate event arrives', () => {
    const queryClient = new QueryClient();
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries');
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    invalidateSpy.mockClear();

    act(() => MockEventSource.instances[0].emit('live', { jobs: [], down: 0, up: 0 }));

    const jobsPrefixCalls = invalidateSpy.mock.calls.filter(
      (c) => Array.isArray(c[0]?.queryKey) && c[0]!.queryKey[0] === 'jobs',
    );
    expect(jobsPrefixCalls).toHaveLength(0);
  });

  // Mirrors ScopePublisher above, but for the throughput opt-in.
  function ThroughputPublisher() {
    useThroughputStream();
    return null;
  }

  it('adds ?throughput=1 to the URL while a child calls useThroughputStream', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>
            <ThroughputPublisher />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(2);
    expect(MockEventSource.instances[0].closed).toBe(true);
    expect(MockEventSource.instances[1].url).toBe('/api/stream?throughput=1');
  });

  it('omits ?throughput= when nothing calls useThroughputStream', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/stream');
  });

  it('combines ?jobs= and ?throughput=1', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <ScopePublisher ids={[1, 2]} />
            <ThroughputPublisher />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    const last = MockEventSource.instances[MockEventSource.instances.length - 1];
    expect(last.closed).toBe(false);
    expect(last.url).toBe('/api/stream?jobs=1,2&throughput=1');
  });

  it('carries no throughput param on a ?job= detail route with no throughput consumer', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs/5']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/stream?job=5');
  });

  // ScopePublisher above (used by every other test in this file) takes its
  // ids as a static prop, already resolved at mount. Overview's real ids
  // come from useJobs (React Query), which starts undefined — so
  // transferRows.map(...) falls back to [] — and only resolves to the real
  // array on a later microtask. AsyncScopePublisher reproduces that timing:
  // an id set that starts empty and becomes real one tick after mount, the
  // same shape a mocked-but-still-async query has.
  function AsyncScopePublisher({ ids }: { ids: number[] }) {
    const [resolvedIds, setResolvedIds] = useState<number[]>([]);
    useEffect(() => {
      void Promise.resolve().then(() => setResolvedIds(ids));
    }, [ids]);
    useJobScope(resolvedIds);
    return null;
  }

  // Pins the actual cost of Overview's timing (issue #265 review): a static
  // ids prop (every other job-scope test in this file) opens 2 connections
  // (unscoped, then the real `?jobs=...`, per #267) — but an id set that
  // resolves asynchronously, like Overview's, opens a THIRD: unscoped, then
  // an intermediate `?jobs=` (empty, published the instant the scope
  // publisher's own mount effect runs, before its query has resolved), then
  // finally the real `?jobs=1,2,3`. This is a job-scope cost, not a
  // throughput one — see the next test.
  it('costs three connections for an async-resolving job scope, not the two a static prop shows', async () => {
    const queryClient = new QueryClient();
    await act(async () => {
      render(
        <QueryClientProvider client={queryClient}>
          <MemoryRouter initialEntries={['/jobs']}>
            <StreamProvider>
              <AsyncScopePublisher ids={[1, 2, 3]} />
            </StreamProvider>
          </MemoryRouter>
        </QueryClientProvider>,
      );
      // Lets the simulated query-resolution microtask (and the effect it
      // schedules) flush.
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(MockEventSource.instances).toHaveLength(3);
    expect(MockEventSource.instances[0].url).toBe('/api/stream');
    expect(MockEventSource.instances[1].url).toBe('/api/stream?jobs=');
    const last = MockEventSource.instances[MockEventSource.instances.length - 1];
    expect(last.url).toBe('/api/stream?jobs=1,2,3');
    expect(last.closed).toBe(false);
  });

  // The counterpart to the test above: mounting useThroughputStream
  // alongside the same async-resolving scope must NOT add a fourth
  // connection. Both effects fire in the same commit on every render (they
  // are direct siblings, same as on Overview), so wantThroughput's counter
  // update rides along with whichever jobsScope state is current when it
  // fires — the third connection above already exists without it. This is
  // the empirical check behind the #267 comment in stream.tsx: the total
  // cost is bounded by the job-scope's own two extra hops, not by
  // throughput adding a THIRD one on top.
  it('adding useThroughputStream to the same async-resolving scope costs no additional connection', async () => {
    const queryClient = new QueryClient();
    await act(async () => {
      render(
        <QueryClientProvider client={queryClient}>
          <MemoryRouter initialEntries={['/jobs']}>
            <StreamProvider>
              <AsyncScopePublisher ids={[1, 2, 3]} />
              <ThroughputPublisher />
            </StreamProvider>
          </MemoryRouter>
        </QueryClientProvider>,
      );
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(MockEventSource.instances).toHaveLength(3);
    const last = MockEventSource.instances[MockEventSource.instances.length - 1];
    expect(last.url).toBe('/api/stream?jobs=1,2,3&throughput=1');
    expect(last.closed).toBe(false);
  });

  // Mirrors ThroughputPublisher above, but for the manual-search opt-in
  // (issue #58).
  function SearchPublisher({ id }: { id: string | undefined }) {
    useSearchStream(id);
    return null;
  }

  it('adds ?search=<id> once a child calls useSearchStream with a defined id', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>
            <SearchPublisher id="deadbeefdeadbeefdeadbeefdeadbeef" />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(2);
    expect(MockEventSource.instances[0].closed).toBe(true);
    expect(MockEventSource.instances[1].url).toBe('/api/stream?search=deadbeefdeadbeefdeadbeefdeadbeef');
  });

  it('composes ?search= with ?jobs= and ?throughput=1, and the bare endpoint is unaffected when unused', () => {
    // The existing bare-/api/stream assertion (first test in this file)
    // already pins that a connection with none of the four scopes still
    // opens plain; this test is the composition side: all four axes at once.
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <ScopePublisher ids={[1, 2]} />
            <ThroughputPublisher />
            <SearchPublisher id="deadbeefdeadbeefdeadbeefdeadbeef" />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    const last = MockEventSource.instances[MockEventSource.instances.length - 1];
    expect(last.closed).toBe(false);
    expect(last.url).toBe('/api/stream?jobs=1,2&throughput=1&search=deadbeefdeadbeefdeadbeefdeadbeef');
  });

  it('reopens the connection when the published search id changes, and drops the param once unmounted', () => {
    const queryClient = new QueryClient();
    function Toggle() {
      const [id, setId] = useState<string | undefined>('deadbeefdeadbeefdeadbeefdeadbeef');
      return (
        <>
          <SearchPublisher id={id} />
          <button onClick={() => setId('beefdeadbeefdeadbeefdeadbeefdead')}>swap</button>
          <button onClick={() => setId(undefined)}>clear</button>
        </>
      );
    }
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>
            <Toggle />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances[MockEventSource.instances.length - 1].url).toBe(
      '/api/stream?search=deadbeefdeadbeefdeadbeefdeadbeef',
    );

    act(() => screen.getByText('swap').click());
    expect(MockEventSource.instances[MockEventSource.instances.length - 1].url).toBe(
      '/api/stream?search=beefdeadbeefdeadbeefdeadbeefdead',
    );

    act(() => screen.getByText('clear').click());
    expect(MockEventSource.instances[MockEventSource.instances.length - 1].url).toBe('/api/stream');
  });

  it('folds a `search` frame into queryKeys.search(id), merging groups by id rather than replacing the session', () => {
    const queryClient = new QueryClient();
    const id = 'deadbeefdeadbeefdeadbeefdeadbeef';
    queryClient.setQueryData<SearchSession>(queryKeys.search(id), {
      id,
      query: 'in rainbows',
      startedAt: '2026-01-01T00:00:00Z',
      done: false,
      streaming: true,
      total: 1,
      groups: [{
        id: 'g1', peer: 'lars', folder: 'f', title: 'In Rainbows', parent: 'Radiohead',
        trackCount: 10, sizeBytes: 100, freeUploadSlot: true, queueLength: 0, uploadSpeed: 1000,
        score: 0.5, files: [],
      }],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>
            <SearchPublisher id={id} />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[MockEventSource.instances.length - 1].emit('search', {
      id,
      seq: 1,
      total: 2,
      done: false,
      streaming: true,
      groups: [{
        id: 'g2', peer: 'other', folder: 'f2', title: 'Kid A', parent: 'Radiohead',
        trackCount: 8, sizeBytes: 200, freeUploadSlot: false, queueLength: 3, uploadSpeed: 500,
        score: 0.4, files: [],
      }],
    }));

    const session = queryClient.getQueryData<SearchSession>(queryKeys.search(id));
    expect(session?.total).toBe(2);
    expect(session?.groups.map((g) => g.id)).toEqual(['g1', 'g2']);
  });
});
