import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { ModuleStatus, StatusReport } from '../api/types';
import { t } from '../strings';
import Health from './Health';

afterEach(() => vi.unstubAllGlobals());

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function stubFetchFailing() {
  vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('boom'))));
}

function makeModule(overrides: Partial<ModuleStatus> = {}): ModuleStatus {
  return {
    lastAttempt: '',
    lastCompleted: '',
    lastSuccess: '',
    lastErrorAt: '',
    lastError: '',
    consecutiveFailures: 0,
    staleDeadline: '',
    live: true,
    ready: true,
    ...overrides,
  };
}

function renderHealth(moduleDetails: StatusReport['moduleDetails']) {
  // A dead refetch on mount would otherwise flip these deterministic
  // module-state cases into an error/stale phase; keep it pending
  // indefinitely so the seeded data is what's asserted on.
  stubFetchIndefinitely();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.status, {
    queued: 0,
    active: 0,
    stalled: 0,
    parked: 0,
    modules: {},
    moduleDetails,
  } satisfies StatusReport);
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Health />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Health loading vs empty', () => {
  it('shows the loading placeholder, not the empty message, before the first fetch resolves', () => {
    // No setQueryData call: useStatus() has never resolved, matching the
    // real state during the first fetch. Rendering the empty message here
    // would assert "no modules reported" about a system that simply hasn't
    // answered yet.
    stubFetchIndefinitely();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Health />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getAllByText(t.query.loading).length).toBeGreaterThan(0);
    expect(screen.queryByText(t.health.empty, { exact: false })).not.toBeInTheDocument();
  });

  it('shows the empty message once the fetch resolves with no modules', () => {
    renderHealth({});
    // Asserts the modules region specifically rendered its empty state, not
    // merely that some region somewhere isn't loading — uploads/shares are
    // never seeded here, so their own loading line elsewhere on the page is
    // expected and irrelevant to this assertion.
    expect(screen.getByText(t.health.empty, { exact: false })).toBeInTheDocument();
  });

  it('shows the failed line rather than the empty message when /status fails', async () => {
    stubFetchFailing();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Health />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect((await screen.findAllByText(t.query.failed)).length).toBeGreaterThan(0);
    expect(screen.queryByText(t.health.empty, { exact: false })).not.toBeInTheDocument();
  });

  it('shows real status-sourced metric values with a dash for a failed uploads fetch', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/status') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                queued: 2,
                active: 1,
                stalled: 0,
                parked: 0,
                modules: {},
                moduleDetails: {},
              } satisfies StatusReport),
              { status: 200 },
            ),
          );
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Health />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByText('1')).toBeInTheDocument(); // metricActive
    expect(screen.getAllByText('—').length).toBeGreaterThan(0); // uploads/shares rows
  });
});

describe('Health module states', () => {
  it('shows the never-run label for a module with no lastAttempt, not a formatted date', () => {
    renderHealth({ importer: makeModule({ lastAttempt: '' }) });
    expect(screen.getByText(t.health.neverRun)).toBeInTheDocument();
  });

  it('renders a ready module without the unhealthy marker or a failure count', () => {
    renderHealth({ importer: makeModule({ lastAttempt: '2026-07-20T10:00:00Z', ready: true }) });
    const cell = screen.getByTitle('');
    expect(cell.className).not.toMatch(/unhealthy/);
    expect(cell.textContent).not.toContain('consecutive');
  });

  it('marks a not-ready module unhealthy and appends its consecutive failure count independently', () => {
    renderHealth({
      importer: makeModule({
        lastAttempt: '2026-07-20T10:00:00Z',
        ready: false,
        consecutiveFailures: 3,
        lastError: 'boom',
      }),
    });
    const cell = screen.getByTitle('boom');
    expect(cell.className).toMatch(/unhealthy/);
    expect(cell.textContent).toContain(t.health.consecutiveFailures(3));
  });

  it('renders a ready module with a nonzero failure count without the unhealthy style', () => {
    renderHealth({
      importer: makeModule({
        lastAttempt: '2026-07-20T10:00:00Z',
        ready: true,
        consecutiveFailures: 2,
      }),
    });
    const cell = screen.getByTitle('');
    expect(cell.className).not.toMatch(/unhealthy/);
    expect(cell.textContent).toContain(t.health.consecutiveFailures(2));
  });

  it('marks a not-ready module unhealthy even with zero consecutive failures, and omits the count', () => {
    renderHealth({
      importer: makeModule({
        lastAttempt: '2026-07-20T10:00:00Z',
        ready: false,
        consecutiveFailures: 0,
        lastError: 'boom',
      }),
    });
    const cell = screen.getByTitle('boom');
    expect(cell.className).toMatch(/unhealthy/);
    expect(cell.textContent).not.toContain('consecutive');
  });
});
