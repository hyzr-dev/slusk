import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Job, JobDetail as JobDetailDTO, JobEvent } from '../api/types';
import { FlashProvider } from '../components/chrome/FlashContext';
import StatusBar from '../components/chrome/StatusBar';
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
    year: 1959,
    tracks: 5,
    format: 'FLAC',
    ...overrides,
  };
}

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function stubFetchFailing() {
  vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('boom'))));
}

// Chip buttons carry a count span (e.g. "ACTIVE 1"), so their accessible name
// is the label plus a trailing count — match with a regex rather than the
// bare label, and anchor + require the digit suffix so e.g. "ALL" never
// satisfies some other "ALL \d+" pattern by accident.
function statusChipName(label: string): RegExp {
  return new RegExp(`^${label} \\d+$`);
}

// The status chips and the source chips are two independent axes rendered as
// two ARIA groups on the same row — both have an ALL button, so an unscoped
// query would be ambiguous. Scope to the group that owns the chip you mean.
function statusGroup() {
  return within(screen.getByRole('group', { name: t.columns.status }));
}
function sourceGroup() {
  return within(screen.getByRole('group', { name: t.jobs.sourceFilterLabel }));
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

// FlashContext's message has nowhere to land inside plain renderJobs() —
// StatusBar is what actually renders it — so the one test asserting on a
// flash wraps both in FlashProvider and mounts StatusBar alongside Jobs,
// exactly as Layout does in the real app.
function renderJobsWithChrome(jobs: Job[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  qc.setQueryData(queryKeys.jobs, jobs);
  return render(
    <QueryClientProvider client={qc}>
      <FlashProvider>
        <MemoryRouter>
          <Jobs />
          <StatusBar />
        </MemoryRouter>
      </FlashProvider>
    </QueryClientProvider>,
  );
}

describe('source indicator', () => {
  it('marks a manual job with the source dot and leaves a Lidarr job unmarked', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, source: 'manual', title: 'Rounds' }),
      makeJob({ id: 2, source: 'lidarr', title: 'Dummy' }),
    ]);

    expect(screen.getByTitle(t.source.manual)).toBeInTheDocument();
    expect(screen.queryAllByTitle(t.source.manual)).toHaveLength(1);
  });
});

describe('placeholders for absent fields', () => {
  it('shows an em dash for peer, format, speed and eta when those fields are absent', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({
        id: 1,
        peer: '',
        speed: undefined,
        etaSeconds: undefined,
        format: null,
      }),
    ]);

    expect(screen.getAllByText('—')).toHaveLength(4);
  });
});

describe('queue position rendering', () => {
  it('tags an active job waiting in a peer queue as QU and shows a compact queue position', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([makeJob({ id: 1, status: 'active', queuePosition: 4 })]);

    expect(screen.getByTitle(t.tagTitle.QU)).toHaveTextContent('QU');
    expect(screen.getByText(t.jobs.queueShort(4))).toBeInTheDocument();
    // The one behaviour that actively misinforms rather than merely looking
    // wrong: no bytes move while a job sits in a peer's queue, so its Ticks
    // bar must never flare as though a transfer were live.
    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  // The other half of the same pin: a job that IS genuinely downloading
  // (active, no queue position) must flare exactly one tick, so this view
  // can't silently regress to never flaring at all either.
  it('flares the bar for a genuinely transferring row', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([
      makeJob({ id: 1, status: 'active', queuePosition: undefined, bytesDone: 50, bytesTotal: 100 }),
    ]);

    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(1);
  });

  // Regression: queuePosition comes from the live ListDownloads snapshot
  // whenever an attempt exists, regardless of the job's actual status — a
  // stalled job carrying a stale queue slot from its last attempt must still
  // show its real ST tag and real percentage, not QU.
  it('ignores a stale queuePosition on a non-active job and shows its real tag and progress', () => {
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

    expect(screen.queryByTitle(t.tagTitle.QU)).not.toBeInTheDocument();
    expect(screen.getByTitle(t.tagTitle.ST)).toHaveTextContent('ST');
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

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));

    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
    // The ACTIVE chip's own counter still reads 1 even though it's not selected.
    const activeChip = statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.active) });
    expect(within(activeChip).getByText('1')).toBeInTheDocument();
  });

  // The source axis (Manual vs Lidarr) is a second, independent chip group —
  // not drawn in the mock, but jobFilter.ts's SourceFilter is still live code
  // that must stay reachable from this view (see Jobs.tsx's SOURCE_CHIP_ORDER).
  it('filters by source chip', () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual' }),
    ]);

    fireEvent.click(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) }));

    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
  });

  it('shows the empty state when no job matches the filter', () => {
    stubFetchIndefinitely();
    renderJobs([makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr' })]);

    fireEvent.click(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) }));

    expect(screen.getByText(new RegExp(t.jobs.noMatch))).toBeInTheDocument();
  });

  it("the status ALL chip's count reflects the search filter, not the unfiltered job list", () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', artist: 'Miles Davis', status: 'active' }),
      makeJob({ id: 2, title: 'Rounds', artist: 'Four Tet', status: 'failed' }),
      makeJob({ id: 3, title: 'Sound of Silver', artist: 'LCD Soundsystem', status: 'done' }),
    ]);

    fireEvent.change(screen.getByPlaceholderText(t.jobs.searchPlaceholder), { target: { value: 'Tet' } });

    // Only "Rounds" (Four Tet) matches the search text — the status ALL
    // chip's own counter must already read 1, not 3 (jobs.length).
    const allChip = statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.all) });
    expect(within(allChip).getByText('1')).toBeInTheDocument();

    fireEvent.click(allChip);
    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
  });

  it("the status ALL chip's count reflects the source filter too, not the unfiltered job list", () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr', status: 'active' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual', status: 'failed' }),
      makeJob({ id: 3, title: 'Sound of Silver', source: 'manual', status: 'done' }),
    ]);

    fireEvent.click(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) }));

    // Clicking MANUAL leaves 2 jobs — the status ALL chip's own counter must
    // already read 2, not 3 (jobs.length).
    const allChip = statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.all) });
    expect(within(allChip).getByText('2')).toBeInTheDocument();

    fireEvent.click(allChip);
    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.getByText('Sound of Silver')).toBeInTheDocument();
    expect(screen.queryByText('Kind of Blue')).not.toBeInTheDocument();
  });

  // Mirror of the test above, the other way round: the two axes must count
  // by the same rule. Regression — the source ALL bucket used to reuse the
  // status axis's own `allCount` (which respects the *source* filter and
  // ignores *status*), so with a status chip selected it disagreed with
  // MANUAL + LIDARR and clicking it showed more rows than it promised.
  it("the source ALL chip's count reflects the status filter too, not the unfiltered job list", () => {
    stubFetchIndefinitely();
    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', source: 'lidarr', status: 'active' }),
      makeJob({ id: 2, title: 'Rounds', source: 'manual', status: 'active' }),
      makeJob({ id: 3, title: 'Sound of Silver', source: 'manual', status: 'done' }),
    ]);

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.active) }));

    // Clicking ACTIVE leaves 2 jobs (one manual, one lidarr) — the source
    // ALL chip's own counter must already read 2, matching MANUAL (1) +
    // LIDARR (1), not 3 (jobs.length).
    const sourceAllChip = sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.all) });
    expect(within(sourceAllChip).getByText('2')).toBeInTheDocument();
    expect(
      within(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) })).getByText('1'),
    ).toBeInTheDocument();
    expect(
      within(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.lidarr) })).getByText('1'),
    ).toBeInTheDocument();

    fireEvent.click(sourceAllChip);
    expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
    expect(screen.getByText('Rounds')).toBeInTheDocument();
    expect(screen.queryByText('Sound of Silver')).not.toBeInTheDocument();
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

  // The row itself stays clickable for mouse users, and the toggle button must
  // not double-toggle by letting its own click bubble up to that same handler.
  it('toggles from a click on an ordinary cell, and the toggle button does not double-fire', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);
    renderJobs([makeJob({ id: 1, peer: 'flac_hoarder' })], client);

    fireEvent.click(screen.getByText('flac_hoarder'));
    expect(screen.getByRole('button', { name: t.jobs.hideDetails })).toBeInTheDocument();

    // A bubbling click from the button would toggle twice and land back open.
    fireEvent.click(screen.getByRole('button', { name: t.jobs.hideDetails }));
    expect(screen.getByRole('button', { name: t.jobs.showDetails })).toBeInTheDocument();
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

  // Only the expanded row's detail endpoint should ever be hit — the jobs
  // list polls every 3s and can hold ~150 rows, so calling useJobDetail for
  // every one of them (rather than gating on expansion) would mean each poll
  // fans out into a detail request per row.
  it('fetches file detail only for the expanded row', async () => {
    const seen: number[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const m = /^\/api\/jobs\/(\d+)\/detail$/.exec(url);
        if (m) {
          seen.push(Number(m[1]));
          return Promise.resolve(
            new Response(
              JSON.stringify({ id: Number(m[1]), title: 'x', artist: 'y', state: 'DOWNLOADING', attempts: [] }),
              { status: 200 },
            ),
          );
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );

    renderJobs([
      makeJob({ id: 1, title: 'Kind of Blue', peer: 'flac_hoarder' }),
      makeJob({ id: 2, title: 'Rounds', peer: 'other_peer' }),
    ]);

    fireEvent.click(screen.getByText('flac_hoarder'));

    await waitFor(() => expect(seen).toEqual([1]));
  });

  it('flashes a confirmation after cancelling', async () => {
    const jobsPayload = [makeJob({ id: 1, peer: 'flac_hoarder' })];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/jobs') {
          return Promise.resolve(new Response(JSON.stringify(jobsPayload), { status: 200 }));
        }
        if (url === '/api/jobs/1/detail') {
          return Promise.resolve(
            new Response(
              JSON.stringify({ id: 1, title: 'x', artist: 'y', state: 'DOWNLOADING', attempts: [] }),
              { status: 200 },
            ),
          );
        }
        if (url === '/api/jobs/1/cancel' && init?.method === 'POST') {
          return Promise.resolve(new Response(null, { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );

    renderJobsWithChrome(jobsPayload);

    fireEvent.click(screen.getByText('flac_hoarder'));
    fireEvent.click(await screen.findByRole('button', { name: t.jobs.cancel }));

    expect(await screen.findByText(/cancelled/i)).toBeInTheDocument();
  });
});

describe('query state', () => {
  it('shows the loading line, not the empty message, before the first fetch resolves', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Jobs />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
    expect(screen.queryByText(t.jobs.noMatch)).not.toBeInTheDocument();
    expect(screen.getByPlaceholderText(t.jobs.searchPlaceholder)).toBeInTheDocument();
  });

  it('shows the failed line when the fetch never succeeds', async () => {
    stubFetchFailing();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Jobs />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByText(t.query.failed)).toBeInTheDocument();
    expect(screen.queryByText(t.jobs.noMatch)).not.toBeInTheDocument();
  });

  it('keeps showing the last-known jobs, plus a stale notice, when a refetch fails', async () => {
    stubFetchFailing();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderJobs([makeJob({ id: 1, title: 'Kind of Blue' })], client);
    expect(await screen.findByText(t.query.stale)).toBeInTheDocument();
    expect(screen.getByText('Kind of Blue')).toBeInTheDocument();
  });

  it('shows the empty message, and no notice, once the fetch resolves with no jobs', () => {
    renderJobs([]);
    expect(screen.getByText(new RegExp(t.jobs.noMatch))).toBeInTheDocument();
    expect(screen.queryByText(t.query.loading)).not.toBeInTheDocument();
    expect(screen.queryByText(t.query.failed)).not.toBeInTheDocument();
    expect(screen.queryByText(t.query.stale)).not.toBeInTheDocument();
  });

  it('omits the filter chip counts, rather than asserting 0, while the first fetch is failing', async () => {
    stubFetchFailing();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Jobs />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await screen.findByText(t.query.failed);
    // No chip anywhere in either group carries a count span while the query
    // has never resolved — the bare label is the whole accessible name.
    expect(statusGroup().getByRole('button', { name: t.jobs.chipLabel.all })).toBeInTheDocument();
    expect(sourceGroup().getByRole('button', { name: t.jobs.sourceChipLabel.all })).toBeInTheDocument();
  });

  it('shows the filter chip counts once the jobs fetch resolves', () => {
    renderJobs([makeJob({ id: 1, status: 'active' })]);
    expect(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.all) })).toBeInTheDocument();
    expect(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.all) })).toBeInTheDocument();
  });
});

describe('table semantics', () => {
  it('exposes the eight column headers on a row inside the table', () => {
    renderJobs([]);
    const table = screen.getByRole('table');
    expect(within(table).getAllByRole('columnheader')).toHaveLength(8);
    expect(within(table).getByRole('columnheader', { name: t.jobs.gridHead.album })).toBeInTheDocument();
  });

  it('gives every job row eight cells', () => {
    renderJobs([makeJob({ id: 1 }), makeJob({ id: 2 })]);
    const table = screen.getByRole('table');
    // Two data rows plus the header row.
    expect(within(table).getAllByRole('row')).toHaveLength(3);
    expect(within(table).getAllByRole('cell')).toHaveLength(16);
  });

  it('keeps the album link a link rather than absorbing it into the cell role', () => {
    renderJobs([makeJob({ id: 7, title: 'Kind of Blue' })]);
    // The regression this guards is invisible: role="cell" on the <a> itself
    // would still look and click the same, but the anchor would stop being a
    // link to a screen reader.
    const link = screen.getByRole('link', { name: 'Kind of Blue' });
    expect(link.closest('[role="cell"]')).not.toBe(link);
    expect(link.closest('[role="cell"]')).toBeInTheDocument();
  });

  it('renders an expanded row as one cell spanning all eight columns', async () => {
    const user = userEvent.setup();
    renderJobs([makeJob({ id: 3 })]);
    await user.click(screen.getByRole('button', { name: t.jobs.showDetails }));
    const table = screen.getByRole('table');
    const spanning = within(table)
      .getAllByRole('cell')
      .filter((c) => c.getAttribute('aria-colspan') === '8');
    expect(spanning).toHaveLength(1);
  });

  it('keeps the empty state outside the table, which admits only rows', async () => {
    renderJobs([]);
    // EmptyState wraps the message in decorative dashes ("── … ──"), so an
    // exact-text match never hits — same convention as line 214 above.
    const empty = await screen.findByText(new RegExp(t.jobs.noMatch));
    expect(empty.closest('[role="table"]')).toBeNull();
  });
});
