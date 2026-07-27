import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render } from '@testing-library/react';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from './queries';
import { StreamProvider } from './stream';

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

    act(() => MockEventSource.instances[0].emit('live', { jobs: [{ id: 7, bytesDone: 1, bytesTotal: 2 }], down: 500 }));

    expect(queryClient.getQueryData(queryKeys.live)).toMatchObject({
      down: 500,
      scopeJobId: 7,
      jobs: [{ id: 7, bytesDone: 1, bytesTotal: 2 }],
    });
  });

  it('clears the live cache on error, so consumers fall back to REST', () => {
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => MockEventSource.instances[0].emit('live', { jobs: [], down: 1000 }));
    expect(queryClient.getQueryData(queryKeys.live)).toBeDefined();

    // null, not undefined: setQueryData ignores an undefined value, so
    // clearing has to write a real sentinel to notify subscribers. Asserting
    // toBeUndefined() here would pass against a no-op clear that silently
    // left the last frame overlaid — which is the bug this test guards.
    act(() => MockEventSource.instances[0].onerror?.());
    expect(queryClient.getQueryData(queryKeys.live)).toBeNull();
  });

  it('merges new throughput samples into an already-fetched charts cache', () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: [{ at: '2026-07-26T12:00:00Z', bytesPerSecond: 1000, activeTransfers: 1 }],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/']}>
          <StreamProvider>{null}</StreamProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() =>
      MockEventSource.instances[0].emit('live', {
        jobs: [],
        down: 0,
        throughput: [{ at: '2026-07-26T12:00:01Z', bytesPerSecond: 2000, activeTransfers: 2 }],
      }),
    );

    const charts = queryClient.getQueryData(queryKeys.charts) as { throughput: { at: string }[] };
    expect(charts.throughput.map((s) => s.at)).toEqual(['2026-07-26T12:00:00Z', '2026-07-26T12:00:01Z']);
  });
});
