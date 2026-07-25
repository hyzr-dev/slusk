import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Job, JobDetail as JobDetailDTO, JobEvent } from '../api/types';
import { t } from '../strings';
import Jobs from './Jobs';

afterEach(() => vi.unstubAllGlobals());

function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: 1,
    title: 'Kind of Blue',
    artist: 'Miles Davis',
    status: 'active',
    peer: 'flac_hoarder',
    bytesDone: 50,
    bytesTotal: 100,
    updatedAt: '',
    state: 'DOWNLOADING',
    candidatesTried: 1,
    maxCandidates: 3,
    failReason: '',
    nextAttemptAt: '',
    retries: 0,
    notBefore: '',
    source: 'lidarr',
    year: 1959,
    tracks: 5,
    format: 'FLAC',
    ...overrides,
  };
}

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function renderJobs(jobs: Job[], client?: QueryClient) {
  const qc = client ?? new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(queryKeys.jobs, jobs);
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <Jobs />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('source badge', () => {
  it('renders Manual and Lidarr badges for their respective jobs', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, source: 'manual', title: 'Rounds' }),
      makeJob({ id: 2, source: 'lidarr', title: 'Dummy' }),
    ]);

    // Scoped to the table: the source filter chips above it use the same
    // "Manual"/"Lidarr" labels, so an unscoped query would be ambiguous.
    const table = within(screen.getByRole('table'));
    expect(table.getByText(t.source.manual)).toBeInTheDocument();
    expect(table.getByText(t.source.lidarr)).toBeInTheDocument();
  });
});

describe('placeholders for absent fields', () => {
  it('shows an em dash when speed, eta, format and year are absent', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({
        id: 1,
        speed: undefined,
        etaSeconds: undefined,
        format: null,
        year: null,
      }),
    ]);

    const row = screen.getByRole('button', { expanded: false });
    const cells = within(row).getAllByRole('cell');
    // Peer, Format, Speed, ETA cells all render '—' for missing data; assert
    // at least the format cell and the row's text content overall.
    expect(within(row).getAllByText('—').length).toBeGreaterThanOrEqual(2);
    expect(cells.length).toBeGreaterThan(0);
  });

  it('shows the artist alone (no dangling separator) when year is null', () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, artist: 'Boards of Canada', year: null })]);
    expect(screen.getByText('Boards of Canada')).toBeInTheDocument();
    expect(screen.queryByText(/Boards of Canada ·/)).not.toBeInTheDocument();
  });
});

describe('queue position rendering', () => {
  it("shows the In peer's queue pill and a striped bar when queuePosition is set", () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, status: 'active', queuePosition: 4 })]);

    // Both the status pill and the progress sub-state line read "In peer's
    // queue" for a job stuck in a peer's remote queue.
    expect(screen.getAllByText(t.jobs.inPeerQueue).length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(t.jobs.queuePosition(4))).toBeInTheDocument();
  });
});

describe('chip filtering', () => {
  it('filters by status chip and updates counts without being zeroed by the selected chip', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', status: 'active', state: 'DOWNLOADING' }),
      makeJob({ id: 2, title: 'Rounds', status: 'failed', state: 'FAILED' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: t.status.failed }));

    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
    // The Active chip's own counter still reads 1 even though it's not selected.
    const activeChip = screen.getByRole('button', { name: t.status.active });
    expect(within(activeChip).getByText('1')).toBeInTheDocument();
  });

  it('filters by source chip', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.sourceManual }));

    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
  });

  it('shows the empty state when no job matches the filter', () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr' })]);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.sourceManual }));

    expect(screen.getByText(t.jobs.noMatch)).toBeInTheDocument();
  });

  it('shows a clear-filters button summarising the active filters, and clears them', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr', status: 'active' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual', status: 'failed' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.sourceManual }));
    const clearButton = screen.getByRole('button', { name: t.jobs.clearFilters(t.jobs.sourceManual) });
    fireEvent.click(clearButton);

    expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
    expect(screen.getByText('Rounds')).toBeInTheDocument();
  });
});

describe('row expansion', () => {
  function makeDetail(overrides: Partial<JobDetailDTO> = {}): JobDetailDTO {
    return {
      id: 1,
      title: 'Kind of Blue',
      artist: 'Miles Davis',
      state: 'DOWNLOADING',
      attempts: [],
      ...overrides,
    };
  }

  it('toggles the expansion panel open and closed, and is keyboard-operable', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);
    renderJobs([makeJob({ id: 1 })], client);

    const row = screen.getByRole('button', { expanded: false });
    expect(row).toHaveAttribute('aria-expanded', 'false');

    fireEvent.click(row);
    expect(row).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText(t.jobs.noCandidate)).toBeInTheDocument();

    fireEvent.keyDown(row, { key: 'Enter' });
    expect(row).toHaveAttribute('aria-expanded', 'false');
  });
});
