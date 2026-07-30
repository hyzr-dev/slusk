import { useState } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render } from '@testing-library/react';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from './queries';
import { JOBS_CACHE_LIMIT, StreamProvider, useJobScope, useThroughputStream } from './stream';

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
// produces on each 15s poll/rerender even when the page's ids haven't
// actually changed.
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
  // does on its 15s poll), so useJobScope must dedupe on the joined ids
  // string, not on array identity — otherwise every poll would reopen the
  // connection in a loop.
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

  // Issue #276: Overview/Jobs republish their scope on every 15s poll as job
  // state churns (see useJobScope), which reopens the connection via this
  // exact dep-array change. That must not blank every row on screen for the
  // gap until the new connection's first frame lands.
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

  it('carries no throughput param on a ?job= detail route', () => {
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

  // Guards the React-batching assumption behind combining useJobScope and
  // useThroughputStream on the same connection (issue #265): mounting both
  // in the same commit must open exactly as many EventSource instances as
  // useJobScope alone, or #267's known double-open would become a
  // triple-open. If this fails, StreamProvider needs to derive the
  // throughput want from `pathname === '/'` instead (like jobIdFromPathname)
  // rather than a child-published counter.
  it('opens the same number of connections combining useJobScope and useThroughputStream as useJobScope alone', () => {
    const soloClient = new QueryClient();
    render(
      <QueryClientProvider client={soloClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <ScopePublisher ids={[1, 2, 3]} />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    const soloCount = MockEventSource.instances.length;

    MockEventSource.instances = [];
    const comboClient = new QueryClient();
    render(
      <QueryClientProvider client={comboClient}>
        <MemoryRouter initialEntries={['/jobs']}>
          <StreamProvider>
            <ScopePublisher ids={[1, 2, 3]} />
            <ThroughputPublisher />
          </StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(MockEventSource.instances).toHaveLength(soloCount);
  });
});
