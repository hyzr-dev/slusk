import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DEFAULT_JOB_PAGE_PARAMS, queryKeys } from '../api/queries';
import type { Job, JobDetail as JobDetailDTO, JobEvent, JobFacets, JobPage } from '../api/types';
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
    // A realistic ISO-8601 value, not '' — this route doesn't sort on it,
    // but an empty string is not something the backend ever actually sends.
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    framedAt: '2026-01-01T00:00:00Z',
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

function facetsFor(jobs: Job[]): JobFacets {
  const status: JobFacets['status'] = {
    all: jobs.length,
    active: 0,
    importing: 0,
    queued: 0,
    stalled: 0,
    failed: 0,
    parked: 0,
    done: 0,
  };
  for (const job of jobs) {
    if (job.state === 'IMPORTING') status.importing += 1;
    else status[job.status] += 1;
  }
  return {
    status,
    source: {
      all: jobs.length,
      manual: jobs.filter((job) => job.source === 'manual').length,
      lidarr: jobs.filter((job) => job.source === 'lidarr').length,
    },
  };
}

function pageFor(jobs: Job[], total = jobs.length, facets = facetsFor(jobs)): JobPage {
  return { jobs, total, facets };
}

function seedPage(qc: QueryClient, page: JobPage, params = DEFAULT_JOB_PAGE_PARAMS) {
  qc.setQueryData(queryKeys.jobsPage(params), page);
}

function renderJobs(jobs: Job[], client?: QueryClient, total = jobs.length, facets = facetsFor(jobs)) {
  const qc = client ?? new QueryClient({ defaultOptions: { queries: { retry: false } } });
  seedPage(qc, pageFor(jobs, total, facets));
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
  seedPage(qc, pageFor(jobs));
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

  // Issue #269: currentCandidateOrder can leave a SELECTING (status
  // 'queued') job pointed at a candidate that already failed, so
  // AlbumBytesDone/Total can be non-zero even though nothing is happening.
  // The '—' label already says so; the tick bar must agree rather than
  // showing a dead attempt's leftover progress next to it.
  it('shows an empty tick bar for a queued job carrying a dead candidate\'s bytes', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([
      makeJob({ id: 1, status: 'queued', state: 'SELECTING', bytesDone: 40, bytesTotal: 100 }),
    ]);

    const pctLabel = container.querySelector('[class*="pct_"]') as HTMLElement;
    expect(pctLabel).toHaveTextContent('—');
    const fill = container.querySelector('[data-fill]') as HTMLElement;
    expect(fill.style.width).toBe('0%');
  });
});

describe('server-owned filters and facets', () => {
  it('shows global status and source facet counts rather than counting the current page', () => {
    stubFetchIndefinitely();
    const facets: JobFacets = {
      status: { all: 42, active: 9, importing: 2, queued: 8, stalled: 7, failed: 6, parked: 5, done: 5 },
      source: { all: 31, manual: 11, lidarr: 20 },
    };
    renderJobs([makeJob()], undefined, 42, facets);

    expect(within(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) })).getByText('6')).toBeInTheDocument();
    expect(within(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) })).getByText('11')).toBeInTheDocument();
  });

  it('requests source and status filters from the server instead of page-local filtering', async () => {
    const requested: string[] = [];
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      requested.push(url);
      const params = new URL(url, 'http://localhost').searchParams;
      const jobs = params.get('source') === 'manual'
        ? [makeJob({ id: 2, title: 'Rounds', source: 'manual', status: 'failed', state: 'FAILED' })]
        : [makeJob()];
      return Promise.resolve(new Response(JSON.stringify(pageFor(jobs)), { status: 200 }));
    }));
    renderJobs([makeJob()]);

    fireEvent.click(sourceGroup().getByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) }));
    await screen.findByText('Rounds');
    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));

    await waitFor(() => expect(requested.some((url) => url.includes('filter=failed') && url.includes('source=manual'))).toBe(true));
  });

  it('resets to page one immediately while modestly debouncing search requests', async () => {
    const requested: string[] = [];
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      requested.push(url);
      const isSecondPage = new URL(url, 'http://localhost').searchParams.get('page') === '1';
      const jobs = isSecondPage ? [makeJob({ id: 13, title: 'Page two' })] : [makeJob()];
      return Promise.resolve(new Response(JSON.stringify(pageFor(jobs, 25)), { status: 200 }));
    }));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, pageFor([makeJob()], 25));
    seedPage(client, pageFor([makeJob({ id: 13, title: 'Page two' })], 25), { ...DEFAULT_JOB_PAGE_PARAMS, page: 1 });
    renderJobs([makeJob()], client, 25);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.nextPage }));
    expect(await screen.findByText('Page two')).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText(t.jobs.searchPlaceholder), { target: { value: 'Miles & Blue' } });

    expect(screen.getByRole('button', { name: t.jobs.pageLabel(1) })).toHaveAttribute('aria-current', 'page');
    expect(requested.some((url) => url.includes('q=Miles'))).toBe(false);
    await waitFor(
      () => expect(requested.some((url) => url.includes('page=0') && url.includes('q=Miles+%26+Blue'))).toBe(true),
      { timeout: 1000 },
    );
  });
});

describe('row expansion', () => {
  // jobDetailDTO carries a whole jobDTO under `job` (issue #268) — see the
  // JobDetail doc comment in types.ts. JobExpansion only ever reads
  // detail.attempts (its header comes from the row's own `job` prop, not
  // from this fetch), so the nested job's fields are unused filler here.
  function makeDetail(attempts: JobDetailDTO['attempts'] = []): JobDetailDTO {
    return {
      job: {
        id: 1,
        title: 'Kind of Blue',
        artist: 'Miles Davis',
        status: 'active',
        peer: '',
        bytesDone: 0,
        bytesTotal: 0,
        createdAt: '2026-01-01T00:00:00Z',
        updatedAt: '2026-01-01T00:00:00Z',
        framedAt: '2026-01-01T00:00:00Z',
        state: 'DOWNLOADING',
        candidatesTried: 0,
        maxCandidates: 3,
        failReason: '',
        nextAttemptAt: '',
        retries: 0,
        notBefore: '',
        source: 'lidarr',
        year: null,
        tracks: null,
        format: null,
      },
      attempts,
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

  it('explains a parked job and offers Retry in the list expansion', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    renderJobs([makeJob({ id: 1, status: 'parked', state: 'PARKED' })], client);

    expect(screen.getByTitle(t.tagTitle.PA)).toHaveTextContent('PA');
    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));

    expect(screen.getByText(t.jobs.parkedExplanation)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.jobs.retry })).toBeInTheDocument();
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
    seedPage(qc, pageFor([makeJob({ id: 1 })]));
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
        if (url.startsWith('/api/jobs?')) {
          return Promise.resolve(new Response(JSON.stringify(pageFor(jobsPayload)), { status: 200 }));
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

describe('sorting and pagination', () => {
  it('makes only ST, ALBUM and PEER sortable, with accessible direction toggles', async () => {
    const requested: string[] = [];
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      requested.push(url);
      const body = url.endsWith('/detail')
        ? { id: 1, title: 'Kind of Blue', artist: 'Miles Davis', state: 'DOWNLOADING', attempts: [] }
        : pageFor([makeJob()]);
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    }));
    renderJobs([makeJob()]);
    const table = screen.getByRole('table');
    const headers = within(table).getAllByRole('columnheader');

    // PROGRESS, SPEED, ETA and TRY are all live fields and none are
    // sortable — only ST, ALBUM and PEER can reorder rows without a row
    // jumping mid-poll (see the comment above the TRY column header in
    // Jobs.tsx).
    expect(headers.filter((header) => within(header).queryByRole('button'))).toHaveLength(3);
    expect(within(headers[3]).queryByRole('button')).toBeNull();
    expect(within(headers[4]).queryByRole('button')).toBeNull();
    expect(within(headers[5]).queryByRole('button')).toBeNull();
    expect(within(headers[6]).queryByRole('button')).toBeNull();
    expect(within(headers[7]).queryByRole('button')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));
    fireEvent.click(within(table).getByRole('button', { name: t.jobs.gridHead.album }));
    expect(screen.queryByRole('button', { name: t.jobs.hideDetails })).not.toBeInTheDocument();
    await waitFor(() => expect(requested.some((url) => url.includes('sort=album') && url.includes('dir=asc'))).toBe(true));
    expect(within(table).getByRole('columnheader', { name: /ALBUM/ })).toHaveAttribute('aria-sort', 'ascending');

    fireEvent.click(within(table).getByRole('button', { name: /ALBUM/ }));
    await waitFor(() => expect(requested.some((url) => url.includes('sort=album') && url.includes('dir=desc'))).toBe(true));
    expect(within(table).getByRole('columnheader', { name: /ALBUM/ })).toHaveAttribute('aria-sort', 'descending');
  });

  it('renders bounded numbered controls, range/total, and clears expansion on page change', async () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, pageFor([makeJob({ id: 1 })], 120));
    seedPage(client, pageFor([makeJob({ id: 13, title: 'Page two' })], 120), { ...DEFAULT_JOB_PAGE_PARAMS, page: 1 });
    renderJobs([makeJob({ id: 1 })], client, 120);

    expect(screen.getByText(t.jobs.resultRange(1, 12, 120))).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.jobs.previousPage })).toBeDisabled();
    expect(screen.getByRole('button', { name: t.jobs.pageLabel(10) })).toBeInTheDocument();
    expect(screen.getAllByText('…')).toHaveLength(1);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));
    expect(screen.getByRole('button', { name: t.jobs.hideDetails })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.jobs.nextPage }));

    expect(await screen.findByText('Page two')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.jobs.hideDetails })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.jobs.previousPage })).toBeEnabled();
  });

  it('resets the page and expansion when a status filter changes', async () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, pageFor([makeJob()], 25));
    seedPage(client, pageFor([makeJob({ id: 13, title: 'Page two' })], 25), { ...DEFAULT_JOB_PAGE_PARAMS, page: 1 });
    seedPage(
      client,
      pageFor([makeJob({ id: 20, title: 'Failed result', status: 'failed', state: 'FAILED' })]),
      { ...DEFAULT_JOB_PAGE_PARAMS, filter: 'failed' },
    );
    renderJobs([makeJob()], client, 25);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.nextPage }));
    expect(await screen.findByText('Page two')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));
    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));

    expect(await screen.findByText('Failed result')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.jobs.pageLabel(1) })).toHaveAttribute('aria-current', 'page');
    expect(screen.queryByRole('button', { name: t.jobs.hideDetails })).not.toBeInTheDocument();
  });

  it('supports comma/period shortcuts but ignores editable targets and modifiers', async () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, pageFor([makeJob()], 25));
    seedPage(client, pageFor([makeJob({ id: 13, title: 'Page two' })], 25), { ...DEFAULT_JOB_PAGE_PARAMS, page: 1 });
    renderJobs([makeJob()], client, 25);

    fireEvent.keyDown(document, { key: '.' });
    expect(await screen.findByText('Page two')).toBeInTheDocument();

    const input = screen.getByPlaceholderText(t.jobs.searchPlaceholder);
    fireEvent.keyDown(input, { key: ',' });
    expect(screen.getByText('Page two')).toBeInTheDocument();
    fireEvent.keyDown(document, { key: ',', ctrlKey: true });
    expect(screen.getByText('Page two')).toBeInTheDocument();
    const editable = document.createElement('div');
    editable.setAttribute('contenteditable', 'true');
    document.body.append(editable);
    fireEvent.keyDown(editable, { key: ',' });
    expect(screen.getByText('Page two')).toBeInTheDocument();
    editable.remove();

    fireEvent.keyDown(document, { key: ',' });
    expect(await screen.findByText('Kind of Blue')).toBeInTheDocument();
  });

  it('clamps to the last valid page after the total shrinks', async () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, pageFor([makeJob()], 25));
    const secondParams = { ...DEFAULT_JOB_PAGE_PARAMS, page: 1 };
    seedPage(client, pageFor([makeJob({ id: 13, title: 'Page two' })], 25), secondParams);
    renderJobs([makeJob()], client, 25);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.nextPage }));
    expect(await screen.findByText('Page two')).toBeInTheDocument();
    act(() => seedPage(client, pageFor([], 1), secondParams));

    await waitFor(() => expect(screen.getByRole('button', { name: t.jobs.pageLabel(1) })).toHaveAttribute('aria-current', 'page'));
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
