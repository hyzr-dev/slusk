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
    // A realistic ISO-8601 timestamp, not '': an empty string would make
    // every job compare equal on createdAt, collapsing the TRANSFERS sort to
    // its id tie-break and hiding a broken or removed sortJobs call. Tests
    // that care about ordering pass an explicit createdAt override instead
    // of relying on this default.
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
  parked: 0,
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
  queryClient.setQueryData(queryKeys.jobsAll, jobsData);
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
    expect(screen.getByText('Parked')).toBeInTheDocument();
    expect(screen.getByText('Done')).toBeInTheDocument();
    // Unlike the old dashboard, failed jobs have no stat cell here — the
    // mock and spec only cover active/queued/stalled/parked/done.
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

  it('ranks active above stalled in TRANSFERS, so an older stalled job never evicts an active one (#233)', () => {
    // 7 active jobs (more than fit alongside any stalled ones once
    // MAX_TRANSFER_ROWS (8) is applied) plus 3 stalled jobs. The stalled jobs'
    // createdAt (2020) is far older than every active job's (2026) — if the
    // panel sorted by age alone rather than status group first, the ancient
    // stalled jobs would win every slot and every active job (including the
    // one "created" most recently, job 1) would be evicted. Active createdAt
    // is itself scrambled relative to array/id order, so the test also still
    // proves the createdAt-ascending ordering *within* the active group.
    const active: Job[] = Array.from({ length: 7 }, (_, i) => {
      const n = i + 1;
      return {
        ...baseJob,
        id: n,
        title: `Active ${n}`,
        status: 'active',
        createdAt: `2026-07-01T10:${String(8 - n).padStart(2, '0')}:00Z`,
      };
    });
    const stalled: Job[] = [
      { ...baseJob, id: 8, title: 'Stalled 8', status: 'stalled', createdAt: '2020-01-01T00:03:00Z' },
      { ...baseJob, id: 9, title: 'Stalled 9', status: 'stalled', createdAt: '2020-01-01T00:01:00Z' },
      { ...baseJob, id: 10, title: 'Stalled 10', status: 'stalled', createdAt: '2020-01-01T00:02:00Z' },
    ];
    renderOverview([...active, ...stalled]);

    const rowTitles = Array.from(
      document.querySelectorAll(`[class*="transferRow"] [class*="transferTitle"]`),
    ).map((el) => el.textContent);

    // All 7 active jobs first, oldest-createdAt-first (Active 7 through
    // Active 1), then the single remaining slot goes to the oldest stalled
    // job (Stalled 9) — never to an active job being displaced.
    expect(rowTitles).toEqual([
      'Active 7', 'Active 6', 'Active 5', 'Active 4', 'Active 3', 'Active 2', 'Active 1', 'Stalled 9',
    ]);
    expect(screen.queryByText('Stalled 8')).not.toBeInTheDocument();
    expect(screen.queryByText('Stalled 10')).not.toBeInTheDocument();
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
