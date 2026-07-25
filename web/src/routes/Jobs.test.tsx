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
  return within(screen.getByRole('group', { name: t.jobs.sourceLabel }));
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
    renderJobs([makeJob({ id: 1, status: 'active', queuePosition: 4 })]);

    expect(screen.getByTitle(t.tagTitle.QU)).toHaveTextContent('QU');
    expect(screen.getByText(t.jobs.queueShort(4))).toBeInTheDocument();
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
