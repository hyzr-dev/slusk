import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { ChartsReport, Job, JobFacets, JobPage, JobPageParams, JobStatus, StatusReport } from '../api/types';
import { t } from '../strings';
import Overview from './Overview';

afterEach(() => vi.unstubAllGlobals());

// Mirrors the exact params Overview.tsx passes to useJobs (issue #268) — the
// query key has to match for setQueryData to seed the cache useJobs reads.
const TRANSFER_PARAMS: JobPageParams = {
  page: 0,
  filter: 'inflight',
  sort: 'transfer',
  dir: 'asc',
  source: 'all',
  q: '',
  pageSize: 8,
};

// Mirrors the params Overview.tsx passes for the recently-finished panel.
const FINISHED_PARAMS: JobPageParams = {
  page: 0,
  filter: 'finished',
  sort: 'recent',
  dir: 'desc',
  source: 'all',
  q: '',
  pageSize: 5,
  skipFacets: true,
};

// Mirrors the params Overview.tsx passes for the FAILED panel (#310, review
// follow-up: filter is 'failures', the state-keyed predicate, not 'failed').
const FAILED_PARAMS: JobPageParams = {
  page: 0,
  filter: 'failures',
  sort: 'recent',
  dir: 'desc',
  source: 'all',
  q: '',
  pageSize: 8,
  skipFacets: true,
};

function makeJob(id: number, title: string, artist: string, status: JobStatus): Job {
  return {
    id,
    title,
    artist,
    status,
    peer: status === 'active' ? 'someuser' : '',
    bytesDone: status === 'active' ? 50 : 0,
    bytesTotal: status === 'active' ? 100 : 0,
    createdAt: '2026-07-01T10:00:00Z',
    updatedAt: '2026-07-01T10:00:00Z',
    framedAt: '2026-07-01T10:00:00Z',
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

// Facets only need to be plausible — Overview reads exactly one field from
// them (facets.status.done, issue #268); the rest of the stat grid comes
// from /status, not from this page.
function makeFacets(jobs: Job[]): JobFacets {
  return {
    status: {
      all: jobs.length,
      active: 0,
      importing: 0,
      queued: 0,
      stalled: 0,
      failed: 0,
      parked: 0,
      done: jobs.filter((j) => j.status === 'done').length,
    },
    source: { all: jobs.length, manual: 0, lidarr: jobs.length },
  };
}

// Builds the exact JobPage shape GET /api/jobs returns, which is what
// useJobs's cache now holds instead of a raw Job[] (queryKeys.jobsAll). total
// defaults to jobs.length so every existing caller behaves unchanged; pass it
// explicitly to exercise the truncation case, where total exceeds the rows.
function makeJobPage(jobs: Job[], total: number = jobs.length): JobPage {
  return { jobs, total, facets: makeFacets(jobs) };
}

const baseJob = makeJob(1, 'Kind of Blue', 'Miles Davis', 'active');

const jobPage: JobPage = makeJobPage([
  baseJob,
  makeJob(2, 'Song A', 'Artist B', 'active'),
]);

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
  uploadThroughput: [],
};

function renderOverview(
  jobsData: JobPage = jobPage,
  chartsData: ChartsReport | undefined = charts,
  statusData: StatusReport | undefined = status,
  // undefined (the default) seeds an empty, resolved finished page.
  // null is a distinct sentinel meaning "don't seed this key at all" — the
  // finished query then stays pending forever against the hung-fetch stub,
  // which is what a test needs to prove the finished region's gate is its
  // own and can't blank another region (see 'keeps the transfers panel
  // alive...' below). A JS default parameter only fires for undefined, so
  // this distinction would collapse if null reused undefined's meaning.
  finishedData: JobPage | undefined | null = makeJobPage([]),
  // Same undefined/null distinction as finishedData above, for the failed-
  // imports panel (#310): undefined seeds an empty resolved page, null leaves
  // the query pending against the hung-fetch stub.
  failedData: JobPage | undefined | null = makeJobPage([]),
) {
  // A real refetch on mount would otherwise hit the unmocked global fetch;
  // keep it pending indefinitely so the seeded data is what's asserted on.
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.jobsPage(TRANSFER_PARAMS), jobsData);
  if (finishedData !== null) {
    queryClient.setQueryData(queryKeys.jobsPage(FINISHED_PARAMS), finishedData);
  }
  if (failedData !== null) {
    queryClient.setQueryData(queryKeys.jobsPage(FAILED_PARAMS), failedData);
  }
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

// Real-navigation variant of renderOverview, in the style already used for
// Jobs.tsx (Jobs.test.tsx: 'the job title link is keyboard-navigable...') —
// a MemoryRouter with an actual /jobs/:id route rendering a sentinel proves
// navigate() actually fired, rather than mocking useNavigate and asserting
// on the mock's call args.
function renderOverviewWithNavigation(jobsData: JobPage, finishedData: JobPage, failedData: JobPage = makeJobPage([])) {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.jobsPage(TRANSFER_PARAMS), jobsData);
  queryClient.setQueryData(queryKeys.jobsPage(FINISHED_PARAMS), finishedData);
  queryClient.setQueryData(queryKeys.jobsPage(FAILED_PARAMS), failedData);
  queryClient.setQueryData(queryKeys.status, status);
  queryClient.setQueryData(queryKeys.charts, charts);
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={['/']}>
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/jobs/:id" element={<div>Job detail page</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// Issue #292: both panels' rows are clickable (div role="row" with onClick)
// but were unreachable by keyboard — tabIndex -1, no keydown handler. Covers
// both TRANSFERS and RECENTLY FINISHED explicitly: a fix touching only
// TRANSFERS would silently skip the finished panel, whose onClick only
// arrived with #287.
describe('Overview row keyboard access (#292)', () => {
  const transferPage = makeJobPage([makeJob(1, 'Kind of Blue', 'Miles Davis', 'active')]);
  const finishedPage = makeJobPage([makeJob(90, 'Finished Album', 'Some Artist', 'done')]);

  it('gives the TRANSFERS row a tabIndex of 0', () => {
    renderOverviewWithNavigation(transferPage, makeJobPage([]));
    const row = screen.getByText('Kind of Blue').closest('[role="row"]') as HTMLElement;
    expect(row).toHaveAttribute('tabIndex', '0');
  });

  it('gives the RECENTLY FINISHED row a tabIndex of 0', () => {
    renderOverviewWithNavigation(makeJobPage([]), finishedPage);
    const row = screen.getByText('Finished Album').closest('[role="row"]') as HTMLElement;
    expect(row).toHaveAttribute('tabIndex', '0');
  });

  it('navigates to the job detail page on Enter from a focused TRANSFERS row', async () => {
    renderOverviewWithNavigation(transferPage, makeJobPage([]));
    const row = screen.getByText('Kind of Blue').closest('[role="row"]') as HTMLElement;
    row.focus();
    await userEvent.setup().keyboard('{Enter}');
    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });

  it('navigates to the job detail page on Enter from a focused RECENTLY FINISHED row', async () => {
    renderOverviewWithNavigation(makeJobPage([]), finishedPage);
    const row = screen.getByText('Finished Album').closest('[role="row"]') as HTMLElement;
    row.focus();
    await userEvent.setup().keyboard('{Enter}');
    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });

  it('navigates to the job detail page on Space from a focused TRANSFERS row', async () => {
    renderOverviewWithNavigation(transferPage, makeJobPage([]));
    const row = screen.getByText('Kind of Blue').closest('[role="row"]') as HTMLElement;
    row.focus();
    await userEvent.setup().keyboard(' ');
    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });

  it('navigates to the job detail page on Space from a focused RECENTLY FINISHED row', async () => {
    renderOverviewWithNavigation(makeJobPage([]), finishedPage);
    const row = screen.getByText('Finished Album').closest('[role="row"]') as HTMLElement;
    row.focus();
    await userEvent.setup().keyboard(' ');
    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });

  // Guards against an overly broad keydown handler that activates on any key.
  it('does not navigate on a non-activating key from either row', () => {
    renderOverviewWithNavigation(transferPage, finishedPage);
    const transferRow = screen.getByText('Kind of Blue').closest('[role="row"]') as HTMLElement;
    const finishedRow = screen.getByText('Finished Album').closest('[role="row"]') as HTMLElement;

    fireEvent.keyDown(transferRow, { key: 'ArrowDown' });
    fireEvent.keyDown(finishedRow, { key: 'ArrowDown' });

    expect(screen.queryByText('Job detail page')).not.toBeInTheDocument();
  });

  it('still navigates on a mouse click on either row, unchanged', () => {
    renderOverviewWithNavigation(transferPage, makeJobPage([]));
    fireEvent.click(screen.getByText('Kind of Blue').closest('[role="row"]') as HTMLElement);
    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });

  it('still navigates on a mouse click on the RECENTLY FINISHED row, unchanged', () => {
    renderOverviewWithNavigation(makeJobPage([]), finishedPage);
    fireEvent.click(screen.getByText('Finished Album').closest('[role="row"]') as HTMLElement);
    expect(screen.getByText('Job detail page')).toBeInTheDocument();
  });
});

describe('Overview', () => {
  it('renders the four stat cells (#281 restyle)', () => {
    renderOverview();
    expect(screen.getByText(t.overview.statInFlight)).toBeInTheDocument();
    expect(screen.getByText(t.overview.statQueued)).toBeInTheDocument();
    expect(screen.getByText(t.overview.statImported)).toBeInTheDocument();
    expect(screen.getByText(t.overview.statAttention)).toBeInTheDocument();
    // Unlike the old dashboard, failed jobs have no stat cell here — the
    // mock and spec only cover active/queued/imported/attention.
    expect(screen.queryByText('Failed')).not.toBeInTheDocument();
  });

  // #295: the IN FLIGHT subtitle used to say "downloading from N peers", but
  // status.active (main.go's len(ActiveTransfers)) is a transfer count, not a
  // distinct-peer count, and ActiveTransfers spans queued/in_progress/stalled
  // — most of which aren't "downloading". The fix is wording-only: the stat's
  // number must stay exactly status.active.
  it('labels the IN FLIGHT subtitle by what it counts, not by peers (#295)', () => {
    renderOverview(jobPage, charts, { ...status, active: 21 });
    const inFlightCell = screen.getByText(t.overview.statInFlight).parentElement as HTMLElement;
    expect(within(inFlightCell).getByText('21')).toBeInTheDocument();
    expect(within(inFlightCell).getByText('21 active transfers')).toBeInTheDocument();
    expect(within(inFlightCell).queryByText(/peers/i)).not.toBeInTheDocument();
  });

  it('keeps the IN FLIGHT subtitle singular for exactly one active transfer (#295)', () => {
    renderOverview(jobPage, charts, { ...status, active: 1 });
    const inFlightCell = screen.getByText(t.overview.statInFlight).parentElement as HTMLElement;
    expect(within(inFlightCell).getByText('1')).toBeInTheDocument();
    expect(within(inFlightCell).getByText('1 active transfer')).toBeInTheDocument();
    expect(within(inFlightCell).queryByText('1 active transfers')).not.toBeInTheDocument();
  });

  // The governing principle of #268: the frontend shows exactly what the
  // backend returns for this page — no client-side filter, sort or slice
  // anywhere in the path. Seeding an order the server would never actually
  // produce (queued/done rows mixed in, out of transferOrder) and asserting
  // it renders verbatim is what proves there is no leftover client logic
  // silently re-deriving the same result the old jobSort.ts used to.
  it('renders exactly the rows the endpoint returns, in the order given, unmodified', () => {
    const page = makeJobPage([
      makeJob(2, 'Song A', 'Artist B', 'queued'),
      makeJob(1, 'Kind of Blue', 'Miles Davis', 'active'),
      makeJob(3, 'Song C', 'Artist D', 'done'),
    ]);
    renderOverview(page);

    const rowTitles = Array.from(
      document.querySelectorAll(`[class*="transferRow"] [class*="transferTitle"]`),
    ).map((el) => el.textContent);
    expect(rowTitles).toEqual(['Song A', 'Kind of Blue', 'Song C']);
  });

  // IMPORTED 24H sums /api/charts' completedByHour (issue #281 correction) —
  // a rolling 24h window of attempt_succeeded events, not a snapshot of jobs
  // currently in state DONE (facets.status.done), which would be wrong the
  // moment a job leaves DONE.
  it('sums completedByHour for IMPORTED 24H, ignoring facets.status.done', () => {
    const page = makeJobPage([baseJob]);
    page.facets.status.done = 999; // must be ignored
    renderOverview(page, {
      ...charts,
      completedByHour: [
        { hour: '2026-07-01T09:00:00Z', count: 3 },
        { hour: '2026-07-01T10:00:00Z', count: 4 },
      ],
    });

    const importedCell = screen.getByText(t.overview.statImported).closest('[class*="statCell"]') as HTMLElement;
    expect(importedCell.textContent).toContain('7');
    expect(importedCell.textContent).not.toContain('999');
  });

  it('renders the TRANSFERS, THROUGHPUT and RECONCILE panels with seeded data', () => {
    renderOverview();
    expect(screen.getByText('TRANSFERS')).toBeInTheDocument();
    expect(screen.getByText('THROUGHPUT')).toBeInTheDocument();
    expect(screen.getByText('RECONCILE')).toBeInTheDocument();
    // Proves jobs and chart data actually reach the new markup, not just
    // that the section headers render. jobPage seeds two jobs with total
    // defaulting to jobs.length, so the meta is untruncated.
    expect(screen.getByText(t.overview.inFlightCountMeta(2))).toBeInTheDocument();
    expect(screen.getByText('1 matched')).toBeInTheDocument();
  });

  it('shows the empty reconcile state when the charts report has no passes', () => {
    renderOverview(jobPage, {
      passes: [],
      completedByHour: charts.completedByHour,
      throughput: [],
      uploadThroughput: [],
    });
    expect(screen.getByText('── No pass history yet ──')).toBeInTheDocument();
  });

  it('renders independently-scaled download and upload throughput charts', () => {
    renderOverview(jobPage, {
      ...charts,
      throughput: [
        { at: '2026-07-01T10:00:00Z', bytesPerSecond: 1024, activeTransfers: 1 },
        { at: '2026-07-01T10:00:01Z', bytesPerSecond: 4096, activeTransfers: 2 },
      ],
      uploadThroughput: [
        { at: '2026-07-01T10:00:00Z', bytesPerSecond: 2048, activeTransfers: 1 },
        { at: '2026-07-01T10:00:01Z', bytesPerSecond: 8192, activeTransfers: 3 },
      ],
    });

    expect(screen.getByText(t.overview.downloadThroughput)).toBeInTheDocument();
    expect(screen.getByText(t.overview.uploadThroughput)).toBeInTheDocument();
    expect(screen.getByRole('img', {
      name: t.overview.downloadThroughputAriaLabel('4 KB/s'),
    })).toBeInTheDocument();
    expect(screen.getByRole('img', {
      name: t.overview.uploadThroughputAriaLabel('8 KB/s'),
    })).toBeInTheDocument();
  });

  it('shows a peer-queued job as queued rather than downloading', () => {
    // Job is active but has queuePosition 4 — no bytes are moving.
    renderOverview(makeJobPage([
      { ...baseJob, status: 'active', state: 'DOWNLOADING', queuePosition: 4, speed: 0 },
    ]));
    expect(screen.getByText('QU')).toBeInTheDocument();
    expect(screen.queryByText('DL')).not.toBeInTheDocument();
  });

  // filter: 'inflight' (issue #287) includes IMPORTING jobs directly, so this
  // is now a reachable server state, not just a defensive client-side case.
  it('renders an IMPORTING job with its importing tag and verifying path', () => {
    renderOverview(makeJobPage([
      { ...baseJob, title: 'Importing Album', status: 'importing', state: 'IMPORTING' },
    ]));

    expect(screen.getByText('Importing Album')).toBeInTheDocument();
    expect(screen.getByText('IM')).toBeInTheDocument();
    expect(screen.getByText(t.jobs.verifying)).toBeInTheDocument();
  });

  it('flares the tick bar for a genuinely transferring row but not a peer-queued one', () => {
    // Pinned the same way as Jobs/JobDetail/Shares: a job waiting in a peer's
    // queue is moving no bytes, so its tick bar must never flare as though
    // data were arriving — the one failure mode here that actively misinforms.
    // Scoped per row so one row's state can't be mistaken for the other's.
    renderOverview(makeJobPage([
      { ...baseJob, id: 1, title: 'Transferring Album', status: 'active', state: 'DOWNLOADING', queuePosition: 0, speed: 1000 },
      { ...baseJob, id: 2, title: 'Queued Album', status: 'active', state: 'DOWNLOADING', queuePosition: 4, speed: 0 },
    ]));

    const transferringRow = screen.getByText('Transferring Album').closest('[class*="transferRow"]') as HTMLElement;
    const queuedRow = screen.getByText('Queued Album').closest('[class*="transferRow"]') as HTMLElement;
    expect(transferringRow.querySelectorAll('[data-flare="true"]')).toHaveLength(1);
    expect(queuedRow.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  it('renders an importing row and a waiting row that the old union never selected', () => {
    renderOverview(makeJobPage([
      { ...baseJob, id: 1, title: 'Importing Album', status: 'importing', state: 'IMPORTING', speed: 0 },
      { ...baseJob, id: 2, title: 'Waiting Album', status: 'queued', state: 'DOWNLOADING', speed: 0, bytesDone: 0, bytesTotal: 100 },
    ]));

    expect(screen.getByText('Importing Album')).toBeInTheDocument();
    expect(screen.getByText('Waiting Album')).toBeInTheDocument();
    // IMPORTING replaces the byte counts with the verifying label.
    expect(screen.getByText(t.jobs.verifying)).toBeInTheDocument();
    // Neither row is moving bytes, so neither may flare.
    expect(document.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  it('takes the transfers meta from total, not from the active counter', () => {
    renderOverview(makeJobPage([{ ...baseJob, id: 1, title: 'Only Row', status: 'active' }], 1));
    expect(screen.getByText(t.overview.inFlightCountMeta(1))).toBeInTheDocument();
  });

  it('reveals truncation when total exceeds the rendered rows', () => {
    // pageSize is 8; a total of 12 means four in-flight jobs are not shown.
    const rows = Array.from({ length: 8 }, (_, i) => ({ ...baseJob, id: i + 1, title: `Row ${i + 1}`, status: 'active' as JobStatus }));
    renderOverview(makeJobPage(rows, 12));
    expect(screen.getByText(t.overview.inFlightTruncatedMeta(8, 12))).toBeInTheDocument();
  });

  it('renders a done row and a failed row in the recently finished panel', () => {
    renderOverview(jobPage, charts, status, makeJobPage([
      { ...baseJob, id: 90, title: 'Finished Album', artist: 'Artist A', status: 'done', state: 'DONE', peer: 'someuser', updatedAt: new Date(Date.now() - 12 * 60 * 1000).toISOString() },
      { ...baseJob, id: 91, title: 'Dead Album', artist: 'Artist B', status: 'failed', state: 'FAILED', peer: '', updatedAt: new Date(Date.now() - 41 * 60 * 1000).toISOString() },
    ]));

    expect(screen.getByText(t.overview.finishedHeading)).toBeInTheDocument();
    expect(screen.getByText('Finished Album')).toBeInTheDocument();
    expect(screen.getByText('Dead Album')).toBeInTheDocument();
    // formatAge on updatedAt — a one-hour window can only ever produce minutes.
    expect(screen.getByText('12m')).toBeInTheDocument();
    expect(screen.getByText('41m')).toBeInTheDocument();
  });

  // #333: once the age is day-scaled it can no longer answer "exactly when?",
  // so the tooltip is the only route to the precise instant. jsdom cannot see
  // a tooltip render, but it can hold the attribute to its contract.
  it('carries the exact finish time as a title on the day-scaled WHEN cell', () => {
    const finishedAt = new Date(Date.now() - (108 * 3600 + 23 * 60) * 1000).toISOString();
    renderOverview(jobPage, charts, status, makeJobPage([
      { ...baseJob, id: 92, title: 'Old Album', status: 'done', state: 'DONE', updatedAt: finishedAt },
    ]));

    const when = screen.getByText('4d');
    expect(when).toBeInTheDocument();
    // sv-SE shape, same as formatDateTime everywhere else. Asserting the shape
    // rather than a literal keeps this from breaking under a runner TZ that
    // rolls the date over, the way the format.test.ts date tests do.
    expect(when).toHaveAttribute('title', expect.stringMatching(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/));
  });

  it('omits the title entirely when there is no usable timestamp', () => {
    renderOverview(jobPage, charts, status, makeJobPage([
      { ...baseJob, id: 93, title: 'No Stamp', status: 'done', state: 'DONE', updatedAt: '' },
    ]));

    // Scoped to this row's own WHEN cell: an em dash also renders in every
    // SPEED cell, so an unscoped getByText('—') matches several elements.
    const row = screen.getByText('No Stamp').closest('[role="row"]');
    const when = row?.querySelector('[class*="finishedWhen"]');
    // An em-dash tooltip on an em-dash value would be noise, not precision.
    expect(when).toHaveTextContent('—');
    expect(when).not.toHaveAttribute('title');
  });

  it('shows a window-agnostic empty state when nothing finished recently', () => {
    renderOverview(jobPage, charts, status, makeJobPage([]));
    expect(screen.getByText(`── ${t.overview.noneFinished} ──`)).toBeInTheDocument();
    // The copy must not name the window: the length is a Go constant and no
    // test in either suite could catch the two drifting apart.
    expect(t.overview.noneFinished).not.toMatch(/hour|minute|\d/i);
  });

  it('keeps the transfers panel alive while the finished query is still pending', () => {
    // null means "don't seed queryKeys.jobsPage(FINISHED_PARAMS) at all" — the
    // finished query then has no cache entry and stays pending against the
    // hung-fetch stub, which is what actually exercises a dead/slow poll for
    // that region. (Passing undefined here would hit renderOverview's default
    // parameter and seed a resolved empty page instead — that would prove
    // nothing about gating.)
    renderOverview(jobPage, charts, status, null);
    // A dead poll for one region must never blank another (issue #201).
    expect(screen.getByText(t.overview.transfersHeading)).toBeInTheDocument();
    expect(document.querySelectorAll('[class*="transferRow"]').length).toBeGreaterThan(0);
  });

  it('renders both panels from their own independent queries', () => {
    renderOverview(
      makeJobPage([{ ...baseJob, id: 1, title: 'In Flight', status: 'active' }]),
      charts,
      status,
      makeJobPage([{ ...baseJob, id: 90, title: 'Finished Album', status: 'done', state: 'DONE' }]),
    );
    expect(screen.getByText('In Flight')).toBeInTheDocument();
    expect(screen.getByText('Finished Album')).toBeInTheDocument();
  });

  // #310: the FAILED panel, a third independent panel/query alongside
  // TRANSFERS and RECENTLY FINISHED.
  it('renders the failed panel with a reason, preferring failDetail over failReason', () => {
    renderOverview(jobPage, charts, status, makeJobPage([]), makeJobPage([
      {
        ...baseJob, id: 92, title: 'Bad Import', artist: 'Artist C', status: 'failed', state: 'FAILED',
        peer: '', failReason: 'transfer failed', failDetail: 'Lidarr rejected: track count mismatch',
        updatedAt: new Date(Date.now() - 5 * 60 * 1000).toISOString(),
      },
    ]));

    expect(screen.getByText(t.overview.failedHeading)).toBeInTheDocument();
    expect(screen.getByText('Bad Import')).toBeInTheDocument();
    expect(screen.getByText('Lidarr rejected: track count mismatch')).toBeInTheDocument();
    expect(screen.queryByText('transfer failed')).not.toBeInTheDocument();
    expect(screen.getByText('5m')).toBeInTheDocument();
  });

  it('falls back to failReason, then an em dash, when failDetail is absent', () => {
    renderOverview(jobPage, charts, status, makeJobPage([]), makeJobPage([
      { ...baseJob, id: 92, title: 'Bad Import', status: 'failed', state: 'FAILED', peer: '', failReason: 'transfer failed', failDetail: undefined },
      { ...baseJob, id: 93, title: 'No Reason Import', status: 'failed', state: 'FAILED', peer: '', failReason: '', failDetail: undefined },
    ]));

    expect(screen.getByText('transfer failed')).toBeInTheDocument();
    const noReasonRow = screen.getByText('No Reason Import').closest('[role="row"]') as HTMLElement;
    expect(within(noReasonRow).getByText('—')).toBeInTheDocument();
  });

  it('shows an empty state when nothing has failed', () => {
    renderOverview(jobPage, charts, status, makeJobPage([]), makeJobPage([]));
    expect(screen.getByText(`── ${t.overview.noneFailed} ──`)).toBeInTheDocument();
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
