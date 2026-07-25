import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/queries';
import type { ChartsReport, Job, JobStatus } from '../api/types';
import { formatSpeed } from '../format';
import { t } from '../strings';
import Overview from './Overview';
// Imported for its scoped class names only, to disambiguate text that is
// deliberately reused between the hero pills and the active-downloads rows
// (e.g. "importing" is both a pill label and a row's phase text).
import styles from './Overview.module.css';

function makeJob(overrides: Partial<Job> & { id: number; status: JobStatus }): Job {
  return {
    title: `Job ${overrides.id}`,
    artist: 'Some Artist',
    peer: 'someuser',
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
    ...overrides,
  };
}

// Two distinct "downloading, no queue" jobs so the throughput header's summed
// speed (3.0 MB/s) never collides with an individual row's own speed text
// (1.0 MB/s / 2.0 MB/s), and every row exercises a different phase/meta case.
const percentJob1 = makeJob({ id: 1, status: 'active', bytesDone: 50, bytesTotal: 100, speed: 1024 * 1024 });
const percentJob2 = makeJob({ id: 2, status: 'active', bytesDone: 30, bytesTotal: 200, speed: 2 * 1024 * 1024 });
const queueJob = makeJob({ id: 3, status: 'active', queuePosition: 4, speed: 999999 });
const importingJob = makeJob({ id: 4, status: 'active', state: 'IMPORTING' });
const stalledJob = makeJob({ id: 5, status: 'stalled', speed: 204800 });
const queuedJob = makeJob({ id: 6, status: 'queued' });
const failedJob = makeJob({ id: 7, status: 'failed' });
const orphanedJob = makeJob({ id: 8, status: 'orphaned' });
const doneJob = makeJob({ id: 9, status: 'done' });

const jobs: Job[] = [
  percentJob1,
  percentJob2,
  queueJob,
  importingJob,
  stalledJob,
  queuedJob,
  failedJob,
  orphanedJob,
  doneJob,
];

const charts: ChartsReport = {
  passes: [
    { startedAt: '2026-07-01T10:00:00Z', finishedAt: '2026-07-01T10:00:01Z', searched: 1, matched: 1 },
  ],
  completedByHour: [
    { hour: '2026-07-01T09:00:00Z', count: 3 },
    { hour: '2026-07-01T10:00:00Z', count: 4 },
  ],
  throughput: [],
};

function renderOverview(jobsData: Job[] = jobs, chartsData: ChartsReport | undefined = charts) {
  const queryClient = new QueryClient();
  queryClient.setQueryData(queryKeys.jobs, jobsData);
  queryClient.setQueryData(queryKeys.charts, chartsData);
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Overview />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// A stat card's value/sub live in DOM siblings of the label, not attached to
// it directly — climb from the label to the shared card container so
// `within` can scope assertions and avoid colliding with identical numbers
// elsewhere on the page (e.g. multiple cards showing "1").
function statCard(labelText: string): HTMLElement {
  const label = screen.getByText(labelText);
  return label.closest('div')!.parentElement as HTMLElement;
}

function pill(labelText: string): HTMLElement {
  const pills = document.querySelector(`.${styles.heroPills}`) as HTMLElement;
  return within(pills).getByText(labelText).parentElement as HTMLElement;
}

function activePanel(): HTMLElement {
  return document.querySelector(`.${styles.activePanel}`) as HTMLElement;
}

describe('Overview hero', () => {
  it('renders the plain-language summary built from downloading/completed/queued counts', () => {
    renderOverview();
    // downloadingCount = active(4) - importing(1) = 3, completed24h = 3+4 = 7, queued = 1
    expect(screen.getByText(t.overview.heroSummary(3, 7, 1))).toBeInTheDocument();
  });

  it('shows the three pill counts: downloading, importing, needs you', () => {
    renderOverview();
    expect(within(pill(t.overview.pillDownloading)).getByText('3')).toBeInTheDocument();
    expect(within(pill(t.overview.pillImporting)).getByText('1')).toBeInTheDocument();
    // needs you = stalled(1) + failed(1)
    expect(within(pill(t.overview.pillNeedsYou)).getByText('2')).toBeInTheDocument();
  });
});

describe('Overview stat cards', () => {
  it('shows the active card with the importing sub-label when jobs are importing', () => {
    renderOverview();
    const card = statCard(t.status.active);
    expect(within(card).getByText('4')).toBeInTheDocument();
    expect(within(card).getByText(t.overview.statActiveSubImporting(1))).toBeInTheDocument();
  });

  it('shows the plain "downloading now" sub-label when nothing is importing', () => {
    renderOverview(jobs.filter((j) => j.id !== importingJob.id));
    const card = statCard(t.status.active);
    expect(within(card).getByText(t.overview.statActiveSub)).toBeInTheDocument();
  });

  it('shows queued, stalled and orphaned cards with their sub-labels', () => {
    renderOverview();
    expect(within(statCard(t.status.queued)).getByText('1')).toBeInTheDocument();
    expect(within(statCard(t.status.queued)).getByText(t.overview.statQueuedSub)).toBeInTheDocument();
    expect(within(statCard(t.status.stalled)).getByText('1')).toBeInTheDocument();
    expect(within(statCard(t.status.stalled)).getByText(t.overview.statStalledSub)).toBeInTheDocument();
    expect(within(statCard(t.status.orphaned)).getByText('1')).toBeInTheDocument();
    expect(within(statCard(t.status.orphaned)).getByText(t.overview.statOrphanedSub)).toBeInTheDocument();
  });

  it('sums completedByHour into the 24h completed card, not a client-side "today" cutoff', () => {
    renderOverview();
    const card = statCard(t.overview.statCompletedLabel);
    expect(within(card).getByText('7')).toBeInTheDocument();
    expect(within(card).getByText(t.overview.statCompletedSub)).toBeInTheDocument();
  });
});

describe('Overview active downloads panel', () => {
  it('shows total throughput summed only over actively-transferring jobs, excluding a queued peer slot', () => {
    renderOverview();
    // percentJob1 + percentJob2 speeds only; queueJob is excluded (queuePosition > 0).
    expect(screen.getByText(formatSpeed(1024 * 1024 + 2 * 1024 * 1024))).toBeInTheDocument();
  });

  it('shows "idle" when nothing is actively transferring', () => {
    renderOverview([stalledJob, queuedJob]);
    expect(screen.getByText(t.overview.throughputIdle)).toBeInTheDocument();
  });

  it('renders the percentage phase/meta for a plain downloading job', () => {
    renderOverview();
    const panel = within(activePanel());
    expect(panel.getByText('50%')).toBeInTheDocument();
    expect(panel.getByText(formatSpeed(percentJob1.speed))).toBeInTheDocument();
    expect(panel.getByText('15%')).toBeInTheDocument();
    expect(panel.getByText(formatSpeed(percentJob2.speed))).toBeInTheDocument();
  });

  it('renders the queue phase/meta for a job waiting in a peer queue', () => {
    renderOverview();
    const panel = within(activePanel());
    expect(panel.getByText(t.overview.phaseQueue(4))).toBeInTheDocument();
    expect(panel.getByText(t.overview.metaQueue(4))).toBeInTheDocument();
  });

  it('renders the importing phase/meta for a job in the IMPORTING state', () => {
    renderOverview();
    const panel = within(activePanel());
    expect(panel.getByText(t.overview.phaseImporting)).toBeInTheDocument();
    expect(panel.getByText(t.overview.metaVerifying)).toBeInTheDocument();
  });

  it('renders the stalled phase/meta for a stalled job', () => {
    renderOverview();
    const panel = within(activePanel());
    expect(panel.getByText(t.overview.phaseStalled)).toBeInTheDocument();
    expect(panel.getByText(formatSpeed(stalledJob.speed))).toBeInTheDocument();
  });

  it('excludes queued, done, failed and orphaned jobs from the panel', () => {
    renderOverview();
    expect(screen.queryByText('Job 6')).not.toBeInTheDocument();
    expect(screen.queryByText('Job 9')).not.toBeInTheDocument();
    expect(screen.queryByText('Job 7')).not.toBeInTheDocument();
    expect(screen.queryByText('Job 8')).not.toBeInTheDocument();
  });

  it('shows the empty state when nothing is active or stalled', () => {
    renderOverview([queuedJob, doneJob, failedJob, orphanedJob]);
    expect(screen.getByText(t.overview.empty)).toBeInTheDocument();
  });
});

describe('Overview charts', () => {
  it('renders both chart titles with seeded chart data', () => {
    renderOverview();
    expect(screen.getByText(t.overview.chartPasses)).toBeInTheDocument();
    expect(screen.getByText(t.overview.chartCompleted)).toBeInTheDocument();
  });

  it('shows the empty pass-history state when the charts report has no passes', () => {
    // completedByHour is seeded (as it always is in real operation - the
    // backend zero-fills it to 24 buckets) so only the pass chart's empty
    // state renders here; an empty completedByHour is CumulativeAreaChart's
    // own, separately-covered empty state.
    renderOverview(jobs, { passes: [], completedByHour: charts.completedByHour, throughput: [] });
    expect(screen.getByText(t.overview.noChartData)).toBeInTheDocument();
  });
});
