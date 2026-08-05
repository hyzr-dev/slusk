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
// two ARIA groups on the same row. Only the status axis has an ALL button
// since issue #416, but scoping to the owning group is still the rule here:
// the two axes share a row and an unscoped query reads across both.
function statusGroup() {
  return within(screen.getByRole('group', { name: t.columns.status }));
}
function sourceGroup() {
  return within(screen.getByRole('group', { name: t.jobs.sourceFilterLabel }));
}

function facetsFor(jobs: Job[]): JobFacets {
  const status: JobFacets['status'] = {
    all: jobs.length,
    wanted: 0,
    selecting: 0,
    queued: 0,
    active: 0,
    importing: 0,
    waiting: 0,
    stalled: 0,
    failed: 0,
    parked: 0,
    done: 0,
  };
  for (const job of jobs) {
    if (job.state === 'IMPORTING') status.importing += 1;
    // 'notImported' has no facet of its own (JobStatusFacets predates issue
    // #59) — it doesn't fold into any of the counters above. Tracked as #368;
    // this branch goes away once that lands.
    else if (job.status !== 'notImported') status[job.status] += 1;
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
  // Issue #416: the backend, not a client-side queuePosition check, is now
  // the sole source of truth for "waiting in a peer's queue" — it reports
  // that as its own 'queued' status rather than 'active' plus a
  // queuePosition. An 'active' job renders DL and flares regardless of a
  // leftover queuePosition value.
  it('renders an active job with a non-zero queuePosition as DL, not QU, and still flares the bar', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([
      makeJob({ id: 1, status: 'active', queuePosition: 4, bytesDone: 50, bytesTotal: 100 }),
    ]);

    expect(screen.getByTitle(t.tagTitle.DL)).toHaveTextContent('DL');
    expect(screen.queryByTitle(t.tagTitle.QU)).not.toBeInTheDocument();
    expect(screen.getByText('50%')).toBeInTheDocument();
    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(1);
  });

  it('tags a backend-reported queued job as QU, shows a compact queue position, and never flares', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([makeJob({ id: 1, status: 'queued', queuePosition: 4 })]);

    expect(screen.getByTitle(t.tagTitle.QU)).toHaveTextContent('QU');
    expect(screen.getByText(t.jobs.queueShort(4))).toBeInTheDocument();
    // The one behaviour that actively misinforms rather than merely looking
    // wrong: no bytes move while a job sits in a peer's queue, so its Ticks
    // bar must never flare as though a transfer were live.
    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
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
  // 'selecting' as of issue #416's status split) job pointed at a candidate
  // that already failed, so AlbumBytesDone/Total can be non-zero even though
  // nothing is happening. The '—' label already says so; the tick bar must
  // agree rather than showing a dead attempt's leftover progress next to it.
  it('shows an empty tick bar for a selecting job carrying a dead candidate\'s bytes', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([
      makeJob({ id: 1, status: 'selecting', state: 'SELECTING', bytesDone: 40, bytesTotal: 100 }),
    ]);

    const pctLabel = container.querySelector('[class*="pct_"]') as HTMLElement;
    expect(pctLabel).toHaveTextContent('—');
    const fill = container.querySelector('[data-fill]') as HTMLElement;
    expect(fill.style.width).toBe('0%');
  });

  // Issue #416 decoupled 'queued' from queuePosition: the backend derives it
  // from transfer aggregates, so a queued job can arrive with no position at
  // all. The label used to assert the field was present and would have
  // rendered a literal "Pundefined" — an invented value in the one place the
  // interface is supposed to stay silent.
  it('falls back to the percentage when a queued job carries no queue position', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([
      makeJob({ id: 1, status: 'queued', state: 'DOWNLOADING', bytesDone: 30, bytesTotal: 100 }),
    ]);

    const pctLabel = container.querySelector('[class*="pct_"]') as HTMLElement;
    expect(pctLabel).toHaveTextContent('30%');
    expect(pctLabel.textContent).not.toContain('undefined');
  });

  it('shows the queue position when a queued job carries one', () => {
    stubFetchIndefinitely();
    const { container } = renderJobs([
      makeJob({ id: 1, status: 'queued', state: 'DOWNLOADING', queuePosition: 4, bytesDone: 30, bytesTotal: 100 }),
    ]);

    const pctLabel = container.querySelector('[class*="pct_"]') as HTMLElement;
    expect(pctLabel).toHaveTextContent(t.jobs.queueShort(4));
  });
});

describe('server-owned filters and facets', () => {
  it('shows global status and source facet counts rather than counting the current page', () => {
    stubFetchIndefinitely();
    const facets: JobFacets = {
      status: { all: 42, wanted: 3, selecting: 2, queued: 1, active: 9, importing: 2, waiting: 8, stalled: 7, failed: 6, parked: 5, done: 5 },
      source: { all: 31, manual: 11, lidarr: 20 },
    };
    renderJobs([makeJob()], undefined, 42, facets);

    expect(within(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) })).getByText('6')).toBeInTheDocument();
    // The source axis deliberately carries no counts (issue #416): they cost
    // width the single filter row does not have, and the split is a two-way
    // toggle rather than a population to scan. Its chips are the bare label.
    expect(sourceGroup().getByRole('button', { name: t.jobs.sourceChipLabel.manual })).toBeInTheDocument();
    expect(sourceGroup().queryByRole('button', { name: statusChipName(t.jobs.sourceChipLabel.manual) })).not.toBeInTheDocument();
  });

  // Issue #416: all ten status chips render, in the order that mirrors the
  // backend's sort=st ranking, each with its own facet count.
  it('renders all ten status chips with their facet counts, including the three new pre-transfer ones', () => {
    stubFetchIndefinitely();
    const facets: JobFacets = {
      status: { all: 42, wanted: 3, selecting: 2, queued: 1, active: 9, importing: 2, waiting: 8, stalled: 7, failed: 6, parked: 5, done: 5 },
      source: { all: 31, manual: 11, lidarr: 20 },
    };
    renderJobs([makeJob()], undefined, 42, facets);

    const group = statusGroup();
    expect(group.getAllByRole('button')).toHaveLength(10);
    expect(within(group.getByRole('button', { name: statusChipName(t.jobs.chipLabel.wanted) })).getByText('3')).toBeInTheDocument();
    expect(within(group.getByRole('button', { name: statusChipName(t.jobs.chipLabel.selecting) })).getByText('2')).toBeInTheDocument();
    expect(within(group.getByRole('button', { name: statusChipName(t.jobs.chipLabel.queued) })).getByText('1')).toBeInTheDocument();
    expect(within(group.getByRole('button', { name: statusChipName(t.jobs.chipLabel.waiting) })).getByText('8')).toBeInTheDocument();
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

    fireEvent.click(sourceGroup().getByRole('button', { name: t.jobs.sourceChipLabel.manual }));
    await screen.findByText('Rounds');
    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));

    await waitFor(() => expect(requested.some((url) => url.includes('filter=failed') && url.includes('source=manual'))).toBe(true));
  });

  // Issue #416 dropped the source axis's ALL chip — 'all' is the absence of a
  // source filter, not a third source, and a second ALL beside the status
  // row's own made one word mean two things. Clicking the active chip is now
  // the only way back to unfiltered, so it is the only thing standing between
  // a user and a filter they cannot clear.
  it('clears the source filter when the active source chip is clicked again', async () => {
    const requested: string[] = [];
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      requested.push(url);
      return Promise.resolve(new Response(JSON.stringify(pageFor([makeJob()])), { status: 200 }));
    }));
    renderJobs([makeJob()]);

    const manual = () => sourceGroup().getByRole('button', { name: t.jobs.sourceChipLabel.manual });
    fireEvent.click(manual());
    await waitFor(() => expect(requested.some((url) => url.includes('source=manual'))).toBe(true));
    expect(manual()).toHaveAttribute('aria-pressed', 'true');

    requested.length = 0;
    fireEvent.click(manual());
    await waitFor(() => expect(requested.some((url) => url.includes('source=all'))).toBe(true));
    expect(manual()).toHaveAttribute('aria-pressed', 'false');
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

    fireEvent.click(screen.getByRole('button', { name: t.pager.nextPage }));
    expect(await screen.findByText('Page two')).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText(t.jobs.searchPlaceholder), { target: { value: 'Miles & Blue' } });

    expect(screen.getByRole('button', { name: t.pager.pageLabel(1) })).toHaveAttribute('aria-current', 'page');
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

  // The expansion row reads its source from the row's own job prop, not from
  // the detail fetch. Force search is hidden for a manual job because
  // app.Jobs.ForceSearch can only answer ErrJobNotSearchable (#347, #352);
  // the Lidarr case is asserted too so the guard can't silently become
  // unconditional.
  it('hides Force search on a manual job in the list expansion', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    renderJobs([makeJob({ id: 1, source: 'manual' })], client);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));

    expect(screen.queryByRole('button', { name: t.jobs.forceSearch })).not.toBeInTheDocument();
    // The state-driven actions are untouched by the source guard.
    expect(screen.getByRole('button', { name: t.jobs.cancel })).toBeInTheDocument();
  });

  it('keeps Force search on a Lidarr job in the list expansion', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    renderJobs([makeJob({ id: 1, source: 'lidarr' })], client);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));

    expect(screen.getByRole('button', { name: t.jobs.forceSearch })).toBeInTheDocument();
  });

  // Manual search (issue #376) is a JobActions `extra` slot only JobDetail
  // fills — JobExpansion (the list's expansion row) never passes it, so it
  // must never appear here even though both share the same JobActions bar.
  it('never shows Manual search in the list expansion', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    renderJobs([makeJob({ id: 1, source: 'lidarr' })], client);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));

    expect(screen.queryByText(t.jobs.manualSearch)).not.toBeInTheDocument();
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
    // This view binds ',' and '.', so it — and only it — draws the hint (#434).
    // The glyph is decoration: the accessible name each button is found by
    // stays the plain label.
    expect(screen.getByRole('button', { name: t.pager.previousPage })).toHaveTextContent('[,] PREV');
    expect(screen.getByRole('button', { name: t.pager.nextPage })).toHaveTextContent('NEXT [.]');
    expect(screen.getByRole('button', { name: t.pager.previousPage })).toBeDisabled();
    expect(screen.getByRole('button', { name: t.pager.pageLabel(10) })).toBeInTheDocument();
    expect(screen.getAllByText('…')).toHaveLength(1);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));
    expect(screen.getByRole('button', { name: t.jobs.hideDetails })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.pager.nextPage }));

    expect(await screen.findByText('Page two')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.jobs.hideDetails })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.pager.previousPage })).toBeEnabled();
  });

  it('shows no pager at all when the whole set fits on one page', () => {
    const jobs = [makeJob({ id: 1 }), makeJob({ id: 2, title: 'Second' })];
    renderJobs(jobs);
    expect(screen.queryByLabelText(t.pager.pageLabel(1))).not.toBeInTheDocument();
    expect(screen.queryByText(t.pager.nextPage)).not.toBeInTheDocument();
    expect(screen.queryByText(t.jobs.resultRange(1, jobs.length, jobs.length))).not.toBeInTheDocument();
  });

  it('resets the page and expansion when a status filter changes', async () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, pageFor([makeJob()], 25));
    seedPage(client, pageFor([makeJob({ id: 13, title: 'Page two' })], 25), { ...DEFAULT_JOB_PAGE_PARAMS, page: 1 });
    seedPage(
      client,
      // Spans more than one page so the pager still renders after the filter
      // change — with a single-page set there would be no page button left to
      // read the reset off (see the single-page test above).
      pageFor([makeJob({ id: 20, title: 'Failed result', status: 'failed', state: 'FAILED' })], 25),
      { ...DEFAULT_JOB_PAGE_PARAMS, filter: 'failed' },
    );
    renderJobs([makeJob()], client, 25);

    fireEvent.click(screen.getByRole('button', { name: t.pager.nextPage }));
    expect(await screen.findByText('Page two')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.jobs.showDetails }));
    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));

    expect(await screen.findByText('Failed result')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.pager.pageLabel(1) })).toHaveAttribute('aria-current', 'page');
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

    fireEvent.click(screen.getByRole('button', { name: t.pager.nextPage }));
    expect(await screen.findByText('Page two')).toBeInTheDocument();
    act(() => seedPage(client, pageFor([], 1), secondParams));

    await waitFor(() => expect(screen.getByRole('button', { name: t.pager.pageLabel(1) })).toHaveAttribute('aria-current', 'page'));
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
    expect(sourceGroup().getByRole('button', { name: t.jobs.sourceChipLabel.lidarr })).toBeInTheDocument();
  });

  it('shows the filter chip counts once the jobs fetch resolves', () => {
    renderJobs([makeJob({ id: 1, status: 'active' })]);
    expect(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.all) })).toBeInTheDocument();
    // Only the status axis has an ALL, and only it carries counts. A second
    // ALL on the source axis made one word mean two things on one screen
    // (issue #416); source is now a toggle over MANUAL/LIDARR alone.
    expect(sourceGroup().getByRole('button', { name: t.jobs.sourceChipLabel.lidarr })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^ALL/ })).toHaveLength(1);
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

// Issue #378: the bulk retry acts on the whole filtered set, so what these
// tests pin is scope and honesty — which filters offer it, what the request
// carries, and that the outcome is reported as the server's own numbers
// rather than as a success.
describe('bulk retry', () => {
  const failedFacets = (failed: number, parked = 0): JobFacets => ({
    status: {
      all: failed + parked,
      wanted: 0, selecting: 0, waiting: 0,
      active: 0, importing: 0, queued: 0, stalled: 0, failed, parked, done: 0,
    },
    source: { all: failed + parked, manual: 0, lidarr: failed + parked },
  });

  // Seeds the ALL page plus the failed/parked pages a chip click switches to,
  // so the button's visibility can be exercised without waiting on a fetch.
  function renderWithRetryableFilters(facets: JobFacets) {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const failedJob = makeJob({ id: 20, title: 'Failed result', status: 'failed', state: 'FAILED' });
    const parkedJob = makeJob({ id: 21, title: 'Parked result', status: 'parked', state: 'PARKED' });
    seedPage(qc, pageFor([makeJob()], facets.status.all, facets));
    seedPage(qc, pageFor([failedJob], facets.status.failed, facets), { ...DEFAULT_JOB_PAGE_PARAMS, filter: 'failed' });
    seedPage(qc, pageFor([parkedJob], facets.status.parked, facets), { ...DEFAULT_JOB_PAGE_PARAMS, filter: 'parked' });
    render(
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

  it('offers the action only for the failed and parked filters', async () => {
    stubFetchIndefinitely();
    renderWithRetryableFilters(failedFacets(6, 5));

    expect(screen.queryByRole('button', { name: t.jobs.bulkRetry.button })).not.toBeInTheDocument();

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));
    expect(await screen.findByRole('button', { name: t.jobs.bulkRetry.button })).toBeInTheDocument();

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.done) }));
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: t.jobs.bulkRetry.button })).not.toBeInTheDocument());
  });

  it('hides the action when the filtered set is empty', async () => {
    stubFetchIndefinitely();
    renderWithRetryableFilters(failedFacets(0));

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));
    await screen.findByText('Failed result');
    expect(screen.queryByRole('button', { name: t.jobs.bulkRetry.button })).not.toBeInTheDocument();
  });

  it('states the failed count as an upper bound and the parked count plainly', async () => {
    stubFetchIndefinitely();
    renderWithRetryableFilters(failedFacets(6, 5));

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));
    fireEvent.click(await screen.findByRole('button', { name: t.jobs.bulkRetry.button }));
    expect(within(screen.getByRole('dialog')).getByText(t.jobs.bulkRetry.failedBody(6))).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.jobs.bulkRetry.cancel }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.parked) }));
    fireEvent.click(await screen.findByRole('button', { name: t.jobs.bulkRetry.button }));
    expect(within(screen.getByRole('dialog')).getByText(t.jobs.bulkRetry.parkedBody(5))).toBeInTheDocument();
  });

  it('sends the current filter scope and reports the server’s own counts', async () => {
    const requested: string[] = [];
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      requested.push(`${init?.method ?? 'GET'} ${url}`);
      if (url.startsWith('/api/jobs/retry')) {
        return Promise.resolve(new Response(JSON.stringify({ retried: 42, skipped: 3 }), { status: 200 }));
      }
      return new Promise(() => {});
    }));
    renderWithRetryableFilters(failedFacets(6));

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));
    fireEvent.click(await screen.findByRole('button', { name: t.jobs.bulkRetry.button }));
    fireEvent.click(screen.getByRole('button', { name: t.jobs.bulkRetry.confirm }));

    await waitFor(() =>
      expect(requested).toContain('POST /api/jobs/retry?filter=failed&source=all&q='));
    // StatusBar prefixes a flash with its own ✓ glyph, so match the substring.
    expect(await screen.findByText(new RegExp(t.jobs.bulkRetry.resultFlash(42, 3)))).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  // Scoped to within(dialog), not to the document: the scrim is position:fixed
  // with a z-index, so an error rendered as a sibling of the dialog is painted
  // behind it and invisible — and a document-wide getByText passes on exactly
  // that bug, since jsdom computes no layout.
  it('reports a failed bulk retry inside the still-open dialog', async () => {
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url.startsWith('/api/jobs/retry')) {
        return Promise.resolve(new Response('db exploded', { status: 500 }));
      }
      return new Promise(() => {});
    }));
    renderWithRetryableFilters(failedFacets(6));

    fireEvent.click(statusGroup().getByRole('button', { name: statusChipName(t.jobs.chipLabel.failed) }));
    fireEvent.click(await screen.findByRole('button', { name: t.jobs.bulkRetry.button }));
    fireEvent.click(screen.getByRole('button', { name: t.jobs.bulkRetry.confirm }));

    const dialog = await screen.findByRole('dialog');
    await waitFor(() => expect(within(dialog).getByText(t.jobs.bulkRetry.failed)).toBeInTheDocument());
    // Still open, so the user can retry without re-opening it.
    expect(within(dialog).getByRole('button', { name: t.jobs.bulkRetry.confirm })).toBeInTheDocument();
  });
});
