import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { AppConfig, Job, JobDetail, ModuleStatus, StatusReport } from '../api/types';
import { t } from '../strings';
import Header from './Header';

function makeJob(id: number, title: string): Job {
  return {
    id,
    title,
    artist: 'Some Artist',
    status: 'active',
    peer: '',
    bytesDone: 0,
    bytesTotal: 0,
    updatedAt: '',
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
  };
}

function makeJobDetail(id: number, title: string): JobDetail {
  return { id, title, artist: 'Some Artist', state: 'DOWNLOADING', attempts: [] };
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
  opts: {
    status?: StatusReport;
    config?: AppConfig;
    jobsUpdatedAt?: number;
    jobs?: Job[];
    jobDetail?: JobDetail;
  } = {},
) {
  const client = new QueryClient();
  if (opts.jobs) {
    client.setQueryData(
      queryKeys.jobs,
      opts.jobs,
      opts.jobsUpdatedAt !== undefined ? { updatedAt: opts.jobsUpdatedAt } : undefined,
    );
  } else if (opts.jobsUpdatedAt !== undefined) {
    client.setQueryData(queryKeys.jobs, [], { updatedAt: opts.jobsUpdatedAt });
  }
  if (opts.jobDetail) client.setQueryData(queryKeys.jobDetail(opts.jobDetail.id), opts.jobDetail);
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

  // The static "Job detail" title (t.header.jobDetail.title) is only the
  // eventual fallback shape — with no seeded job data it can't be shown, so
  // these test the header's actual three-tier behaviour: live job, cached
  // detail, then a loading placeholder. See routes/JobDetail.tsx, which used
  // to render its own duplicate heading for the same reason.
  describe('job detail title (/jobs/:id)', () => {
    it('shows a loading placeholder before any job data has arrived', () => {
      renderHeader('/jobs/42');
      expect(screen.getByText(t.jobs.loading)).toBeInTheDocument();
    });

    it('shows the title from the live jobs list once it loads', () => {
      renderHeader('/jobs/42', { jobs: [makeJob(42, 'Kind of Blue')] });
      expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
      expect(screen.getByText(t.header.jobDetail.subtitleWithId('42'))).toBeInTheDocument();
    });

    it('falls back to the cached job-detail title once a job ages out of the live list', () => {
      renderHeader('/jobs/42', { jobDetail: makeJobDetail(42, 'Blue Train') });
      expect(screen.getByText('Blue Train')).toBeInTheDocument();
      expect(screen.getByText(t.header.jobDetail.subtitleWithId('42'))).toBeInTheDocument();
    });
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
