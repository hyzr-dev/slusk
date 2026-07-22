import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/queries';
import type { ChartsReport, Job, JobStatus } from '../api/types';
import Overview from './Overview';

function makeJob(id: number, title: string, artist: string, status: JobStatus): Job {
  return {
    id,
    title,
    artist,
    status,
    peer: status === 'active' ? 'someuser' : '',
    bytesDone: status === 'active' ? 50 : 0,
    bytesTotal: status === 'active' ? 100 : 0,
    updatedAt: '',
    state: 'DOWNLOADING',
    candidatesTried: 1,
    maxCandidates: 3,
    failReason: '',
    nextAttemptAt: '',
    retries: 0,
    notBefore: '',
  };
}

const jobs: Job[] = [
  makeJob(1, 'Kind of Blue', 'Miles Davis', 'active'),
  makeJob(2, 'Song A', 'Artist B', 'queued'),
  makeJob(3, 'Song C', 'Artist D', 'done'),
];

const charts: ChartsReport = {
  passes: [
    { startedAt: '2026-07-01T10:00:00Z', finishedAt: '2026-07-01T10:00:01Z', searched: 1, matched: 1 },
  ],
  completedByHour: [{ hour: '2026-07-01T10:00:00Z', count: 2 }],
};

function renderOverview(chartsData: ChartsReport | undefined = charts) {
  const queryClient = new QueryClient();
  queryClient.setQueryData(queryKeys.jobs, jobs);
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
  it('renders all five status cards, including failed', () => {
    renderOverview();
    expect(screen.getByText('Queued')).toBeInTheDocument();
    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByText('Stalled')).toBeInTheDocument();
    expect(screen.getByText('Done')).toBeInTheDocument();
    expect(screen.getByText('Failed')).toBeInTheDocument();
  });

  it('shows only active jobs in the table, ignoring other statuses', () => {
    renderOverview();
    expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
    expect(screen.queryByText('Song A')).not.toBeInTheDocument();
    expect(screen.queryByText('Song C')).not.toBeInTheDocument();
  });

  it('renders both chart titles with seeded chart data', () => {
    renderOverview();
    expect(screen.getByText('Matched albums per pass · last 20')).toBeInTheDocument();
    expect(screen.getByText('Completed downloads · last 24 h')).toBeInTheDocument();
  });

  it('shows the empty pass-history state when the charts report has no passes', () => {
    renderOverview({ passes: [], completedByHour: [] });
    expect(screen.getByText('No pass history yet')).toBeInTheDocument();
  });
});
