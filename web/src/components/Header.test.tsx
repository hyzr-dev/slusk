import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { AppConfig, ModuleStatus, StatusReport } from '../api/types';
import { t } from '../strings';
import Header from './Header';

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

function makeStatus(wantedSync: Partial<ModuleStatus> = {}): StatusReport {
  return {
    queued: 0,
    active: 0,
    stalled: 0,
    orphaned: 0,
    modules: {},
    moduleDetails: { wanted_sync: makeModule(wantedSync) },
  };
}

// Header only reads pipeline.wantedSyncInterval off the config, so a minimal
// cast avoids constructing the entire AppConfig shape.
function makeConfig(wantedSyncInterval: string | undefined): AppConfig {
  return { pipeline: { wantedSyncInterval } } as unknown as AppConfig;
}

function renderHeader(
  path: string,
  opts: { status?: StatusReport; config?: AppConfig; jobsUpdatedAt?: number } = {},
) {
  const client = new QueryClient();
  if (opts.jobsUpdatedAt !== undefined) {
    client.setQueryData(queryKeys.jobs, [], { updatedAt: opts.jobsUpdatedAt });
  }
  if (opts.status) client.setQueryData(queryKeys.status, opts.status);
  if (opts.config) client.setQueryData(queryKeys.config, opts.config);
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/jobs/:id" element={<Header />} />
          <Route path="*" element={<Header />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Header page title mapping', () => {
  it.each([
    ['/', t.header.overview],
    ['/jobs', t.header.jobs],
    ['/events', t.header.events],
    ['/peers', t.header.peers],
    ['/shares', t.header.shares],
    ['/health', t.header.health],
    ['/settings', t.header.settings],
  ])('shows the title and subtitle for %s', (path, expected) => {
    renderHeader(path);
    expect(screen.getByText(expected.title)).toBeInTheDocument();
    expect(screen.getByText(expected.subtitle)).toBeInTheDocument();
  });

  it('shows the job detail title with the id from the URL for /jobs/:id', () => {
    renderHeader('/jobs/42');
    expect(screen.getByText(t.header.jobDetail.title)).toBeInTheDocument();
    expect(screen.getByText(t.header.jobDetail.subtitleWithId('42'))).toBeInTheDocument();
  });
});

describe('Header live indicator', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows "updated Ns ago" derived from the jobs query freshness, advancing with the ticking clock', () => {
    const now = Date.parse('2026-07-25T12:00:00Z');
    vi.setSystemTime(now);
    renderHeader('/', { jobsUpdatedAt: now });

    expect(screen.getByText(t.header.updatedAgo(0))).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(5000);
    });

    expect(screen.getByText(t.header.updatedAgo(5))).toBeInTheDocument();
  });
});

describe('Header reconcile countdown', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(now);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows minutes remaining until the next wanted-sync pass', () => {
    const lastCompleted = new Date(now - 5 * 60 * 1000).toISOString();
    renderHeader('/', {
      status: makeStatus({ lastCompleted }),
      config: makeConfig('15m0s'),
    });
    expect(screen.getByText(t.header.reconcileIn(10))).toBeInTheDocument();
  });

  it('shows "due now" once the deadline has passed instead of a negative number', () => {
    const lastCompleted = new Date(now - 60 * 60 * 1000).toISOString();
    renderHeader('/', {
      status: makeStatus({ lastCompleted }),
      config: makeConfig('15m0s'),
    });
    expect(screen.getByText(t.header.reconcileDueNow)).toBeInTheDocument();
  });

  it('renders nothing when wanted_sync has never completed', () => {
    renderHeader('/', {
      status: makeStatus({ lastCompleted: '' }),
      config: makeConfig('15m0s'),
    });
    expect(screen.queryByText(/reconcile/)).not.toBeInTheDocument();
  });

  it('renders nothing when wantedSyncInterval is unparseable', () => {
    const lastCompleted = new Date(now - 5 * 60 * 1000).toISOString();
    renderHeader('/', {
      status: makeStatus({ lastCompleted }),
      config: makeConfig('garbage'),
    });
    expect(screen.queryByText(/reconcile/)).not.toBeInTheDocument();
  });

  it('renders nothing when the config has not loaded yet', () => {
    const lastCompleted = new Date(now - 5 * 60 * 1000).toISOString();
    renderHeader('/', {
      status: makeStatus({ lastCompleted }),
    });
    expect(screen.queryByText(/reconcile/)).not.toBeInTheDocument();
  });
});
