import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Job, JobStatus } from '../api/types';
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

function renderOverview() {
  const queryClient = new QueryClient();
  queryClient.setQueryData(queryKeys.jobs, jobs);
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
});
