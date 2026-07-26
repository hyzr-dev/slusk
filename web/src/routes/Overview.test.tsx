import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { ChartsReport, Job, JobStatus, StatusReport } from '../api/types';
import { t } from '../strings';
import Overview from './Overview';

afterEach(() => vi.unstubAllGlobals());

function makeJob(id: number, title: string, artist: string, status: JobStatus): Job {
  return {
    id,
    title,
    artist,
    status,
    peer: status === 'active' ? 'someuser' : '',
    bytesDone: status === 'active' ? 50 : 0,
    bytesTotal: status === 'active' ? 100 : 0,
    createdAt: '',
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

const baseJob = makeJob(1, 'Kind of Blue', 'Miles Davis', 'active');

const jobs: Job[] = [
  baseJob,
  makeJob(2, 'Song A', 'Artist B', 'queued'),
  makeJob(3, 'Song C', 'Artist D', 'done'),
];

const status: StatusReport = {
  queued: 1,
  active: 1,
  stalled: 0,
  orphaned: 0,
  modules: {},
  moduleDetails: {},
};

const charts: ChartsReport = {
  passes: [
    { startedAt: '2026-07-01T10:00:00Z', finishedAt: '2026-07-01T10:00:01Z', searched: 1, matched: 1 },
  ],
  completedByHour: [{ hour: '2026-07-01T10:00:00Z', count: 2 }],
  throughput: [],
};

function renderOverview(
  jobsData: Job[] = jobs,
  chartsData: ChartsReport | undefined = charts,
  statusData: StatusReport | undefined = status,
) {
  // A real refetch on mount would otherwise hit the unmocked global fetch;
  // keep it pending indefinitely so the seeded data is what's asserted on.
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.jobs, jobsData);
  queryClient.setQueryData(queryKeys.status, statusData);
  queryClient.setQueryData(queryKeys.charts, chartsData);
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Overview />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Overview', () => {
  it('renders the five stat cells, with no failed cell', () => {
    renderOverview();
    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByText('Queued')).toBeInTheDocument();
    expect(screen.getByText('Stalled')).toBeInTheDocument();
    expect(screen.getByText('Orphaned')).toBeInTheDocument();
    expect(screen.getByText('Done')).toBeInTheDocument();
    // Unlike the old dashboard, failed jobs have no stat cell here — the
    // mock and spec only cover active/queued/stalled/orphaned/done.
    expect(screen.queryByText('Failed')).not.toBeInTheDocument();
  });

  it('shows only active and stalled jobs in TRANSFERS, ignoring queued and done', () => {
    renderOverview();
    expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
    expect(screen.queryByText('Song A')).not.toBeInTheDocument();
    expect(screen.queryByText('Song C')).not.toBeInTheDocument();
  });

  it('renders the TRANSFERS, THROUGHPUT and RECONCILE panels with seeded data', () => {
    renderOverview();
    expect(screen.getByText('TRANSFERS')).toBeInTheDocument();
    expect(screen.getByText('THROUGHPUT')).toBeInTheDocument();
    expect(screen.getByText('RECONCILE')).toBeInTheDocument();
    // Proves status and chart data actually reach the new markup, not just
    // that the section headers render.
    expect(screen.getByText('1 active')).toBeInTheDocument();
    expect(screen.getByText('1 matched')).toBeInTheDocument();
  });

  it('shows the empty reconcile state when the charts report has no passes', () => {
    renderOverview(jobs, { passes: [], completedByHour: charts.completedByHour, throughput: [] });
    expect(screen.getByText('── No pass history yet ──')).toBeInTheDocument();
  });

  it('shows a peer-queued job as queued rather than downloading', () => {
    // Job is active but has queuePosition 4 — no bytes are moving.
    renderOverview([
      { ...baseJob, status: 'active', state: 'DOWNLOADING', queuePosition: 4, speed: 0 },
    ]);
    expect(screen.getByText('QU')).toBeInTheDocument();
    expect(screen.queryByText('DL')).not.toBeInTheDocument();
  });

  it('flares the tick bar for a genuinely transferring row but not a peer-queued one', () => {
    // Pinned the same way as Jobs/JobDetail/Shares: a job waiting in a peer's
    // queue is moving no bytes, so its tick bar must never flare as though
    // data were arriving — the one failure mode here that actively misinforms.
    // Scoped per row so one row's state can't be mistaken for the other's.
    renderOverview([
      { ...baseJob, id: 1, title: 'Transferring Album', status: 'active', state: 'DOWNLOADING', queuePosition: 0, speed: 1000 },
      { ...baseJob, id: 2, title: 'Queued Album', status: 'active', state: 'DOWNLOADING', queuePosition: 4, speed: 0 },
    ]);

    const transferringRow = screen.getByText('Transferring Album').closest('[class*="transferRow"]') as HTMLElement;
    const queuedRow = screen.getByText('Queued Album').closest('[class*="transferRow"]') as HTMLElement;
    expect(transferringRow.querySelectorAll('[data-flare="true"]')).toHaveLength(1);
    expect(queuedRow.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });
});

describe('Overview query state', () => {
  it('shows the failed line and dashes in the stat grid, not zeros, when nothing has ever loaded', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('boom'))));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Overview />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect((await screen.findAllByText(t.query.failed)).length).toBeGreaterThan(0);
    expect(screen.queryByText(t.overview.empty, { exact: false })).not.toBeInTheDocument();
    // Every stat cell shows the "unknown" placeholder rather than a claimed 0.
    expect(screen.queryByText('0')).not.toBeInTheDocument();
    expect(screen.getAllByText('—').length).toBeGreaterThan(0);
  });
});
