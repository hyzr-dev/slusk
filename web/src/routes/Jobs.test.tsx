import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
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

// Status chip buttons carry a count span (e.g. "Failed 1"), so their
// accessible name is the label plus a trailing count — match with a regex
// rather than the bare label, and anchor + require the digit suffix so e.g.
// "All" (the source chip, no count) never satisfies the "All \d+" pattern.
function statusChipName(label: string): RegExp {
  return new RegExp(`^${label} \\d+$`);
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
  it('shows an em dash in the peer, format, speed and ETA cells when those fields are absent', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({
        id: 1,
        peer: '',
        speed: undefined,
        etaSeconds: undefined,
        format: null,
        year: null,
      }),
    ]);

    // Row 0 is the header row; row 1 is the one data row.
    const row = screen.getAllByRole('row')[1];
    const cells = within(row).getAllByRole('cell');
    // status, album, peer, format, progress, speed, eta, retries, chevron
    expect(cells).toHaveLength(9);
    expect(cells[2]).toHaveTextContent('—'); // peer
    expect(cells[3]).toHaveTextContent('—'); // format
    expect(cells[5]).toHaveTextContent('—'); // speed
    expect(cells[6]).toHaveTextContent('—'); // eta
  });

  it('shows the artist alone (no dangling separator) when year is null', () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, artist: 'Boards of Canada', year: null })]);
    expect(screen.getByText('Boards of Canada')).toBeInTheDocument();
    expect(screen.queryByText(/Boards of Canada ·/)).not.toBeInTheDocument();
  });
});

describe('queue position rendering', () => {
  it("shows the In peer's queue pill and a distinct sub-state line when queuePosition is set on an active job", () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, status: 'active', queuePosition: 4 })]);

    expect(screen.getByText(t.jobs.inPeerQueue)).toBeInTheDocument();
    expect(screen.getByText(t.jobs.queuedAtPeer)).toBeInTheDocument();
    expect(screen.getByText(t.jobs.queuePosition(4))).toBeInTheDocument();
  });

  // Regression: queuePosition comes from the live ListDownloads snapshot
  // whenever an attempt exists, regardless of the job's actual status — a
  // stalled job carrying a stale queue slot from its last attempt must still
  // show its real Stalled pill and real percentage, not "In peer's queue".
  it('ignores a stale queuePosition on a non-active job and shows its real progress instead', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({
        id: 1,
        status: 'stalled',
        state: 'DOWNLOADING',
        queuePosition: 3,
        bytesDone: 25,
        bytesTotal: 100,
      }),
    ]);

    // Neither the pill nor the progress cell falls into the "In peer's
    // queue" rendering path just because a stale queuePosition is present.
    expect(screen.queryByText(t.jobs.inPeerQueue)).not.toBeInTheDocument();
    expect(screen.queryByText(t.jobs.queuedAtPeer)).not.toBeInTheDocument();
    expect(screen.getByText('25%')).toBeInTheDocument();
  });
});

describe('chip filtering', () => {
  it('filters by status chip and updates counts without being zeroed by the selected chip', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', status: 'active', state: 'DOWNLOADING' }),
      makeJob({ id: 2, title: 'Rounds', status: 'failed', state: 'FAILED' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: statusChipName(t.status.failed) }));

    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
    // The Active chip's own counter still reads 1 even though it's not selected.
    const activeChip = screen.getByRole('button', { name: statusChipName(t.status.active) });
    expect(within(activeChip).getByText('1')).toBeInTheDocument();
  });

  it('filters by source chip', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: t.source.manual }));

    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
  });

  it('shows the empty state when no job matches the filter', () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr' })]);

    fireEvent.click(screen.getByRole('button', { name: t.source.manual }));

    expect(screen.getByText(t.jobs.noMatch)).toBeInTheDocument();
  });

  it('shows a clear-filters button summarising the active filters, and clears them', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr', status: 'active' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual', status: 'failed' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: t.source.manual }));
    const clearButton = screen.getByRole('button', { name: t.jobs.clearFilters(t.source.manual) });
    fireEvent.click(clearButton);

    expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
    expect(screen.getByText('Rounds')).toBeInTheDocument();
  });

  it("the All chip's count reflects the source and search filters, not the unfiltered job list", () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr', status: 'active' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual', status: 'failed' }),
      makeJob({ id: 3, title: 'Sound of Silver', source: 'manual', status: 'done' }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: t.source.manual }));

    // Clicking "All" with Manual selected shows exactly the 2 manual jobs —
    // the chip's own counter must already read 2, not 3 (jobs.length).
    const allChip = screen.getByRole('button', { name: statusChipName(t.jobs.all) });
    expect(within(allChip).getByText('2')).toBeInTheDocument();

    fireEvent.click(allChip);
    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.getByText('Sound of Silver')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
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

  it('toggles the expansion panel open and closed on click', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);
    renderJobs([makeJob({ id: 1 })], client);

    const toggle = screen.getByRole('button', { name: t.jobs.showDetails });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');

    fireEvent.click(toggle);
    expect(screen.getByRole('button', { name: t.jobs.hideDetails })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(screen.getByText(t.jobs.noCandidate)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.jobs.hideDetails }));
    expect(screen.getByRole('button', { name: t.jobs.showDetails })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  // Regression: the row used to be a role="button" tr with an onKeyDown that
  // called preventDefault() on Enter, which also fired for a focused child
  // <Link> (the keydown bubbles), cancelling the browser's own Enter-on-link
  // activation before it could navigate. The row toggle is now a real
  // <button>, so Enter on it activates via native button semantics.
  it('is keyboard-operable: Enter on the toggle button expands the row', async () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);
    renderJobs([makeJob({ id: 1 })], client);

    const user = userEvent.setup();
    const toggle = screen.getByRole('button', { name: t.jobs.showDetails });
    toggle.focus();
    await user.keyboard('{Enter}');

    expect(screen.getByRole('button', { name: t.jobs.hideDetails })).toBeInTheDocument();
    expect(screen.getByText(t.jobs.noCandidate)).toBeInTheDocument();
  });

  // The job title Link sits inside the same clickable row; it must remain
  // independently keyboard-navigable to /jobs/:id rather than being
  // intercepted by the row's own toggle behaviour.
  it('the job title link is keyboard-navigable to the detail page', async () => {
    stubFetchIndefinitely();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(queryKeys.jobs, [makeJob({ id: 1 })]);
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={['/jobs']}>
          <Routes>
            <Route path="/jobs" element={<Jobs />} />
            <Route path="/jobs/:id" element={<div>Job detail page</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const user = userEvent.setup();
    const link = screen.getByRole('link', { name: 'Kind of Blue' });
    link.focus();
    await user.keyboard('{Enter}');

    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });
});
