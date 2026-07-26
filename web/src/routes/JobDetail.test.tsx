import { QueryClient, QueryClientProvider, keepPreviousData } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Job, JobDetail as JobDetailDTO, JobEvent, TransferDetail } from '../api/types';
import { t } from '../strings';
import JobDetail from './JobDetail';

afterEach(() => vi.unstubAllGlobals());

function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: 1,
    title: 'Kind of Blue',
    artist: 'Miles Davis',
    status: 'failed',
    peer: '',
    bytesDone: 0,
    bytesTotal: 0,
    // A realistic ISO-8601 value, not '' — this route doesn't sort on it,
    // but an empty string is not something the backend ever actually sends.
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    state: 'FAILED',
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

function renderJobDetail(path: string, client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/jobs/:id" element={<JobDetail />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('retry visibility', () => {
  // Neither test needs a real network response: the query cache is seeded
  // directly, and fetch is stubbed to a promise that never settles so the
  // inevitable background refetch (staleTime defaults to 0) can't disturb it.
  function stubFetchIndefinitely() {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  }

  it('shows Retry when the live job reports FAILED, even if the cached detail does not', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'FAILED' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail({ state: 'DOWNLOADING' }));
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.getByRole('button', { name: t.jobs.retry })).toBeInTheDocument();
  });

  it('shows Retry when the cached detail reports FAILED, even if the live job does not', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail({ state: 'FAILED' }));
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.getByRole('button', { name: t.jobs.retry })).toBeInTheDocument();
  });

  it('hides Retry when neither source reports FAILED', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail({ state: 'DOWNLOADING' }));
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.queryByRole('button', { name: t.jobs.retry })).not.toBeInTheDocument();
  });

  // PARKED is the other retry-eligible state (issue #60) — JobDetail
  // previously only checked FAILED here.
  it('shows Retry when the live job reports PARKED', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'PARKED', status: 'parked' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail({ state: 'PARKED' }));
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.getByRole('button', { name: t.jobs.retry })).toBeInTheDocument();
    expect(screen.getByText(t.jobs.parkedExplanation)).toBeInTheDocument();
  });
});

describe('delete action', () => {
  it('requires a second click before firing the delete request', async () => {
    const fetchMock = vi.fn(() => new Promise(() => {}));
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    const deleteButton = screen.getByRole('button', { name: t.jobs.delete });
    fireEvent.click(deleteButton);
    expect(fetchMock).not.toHaveBeenCalledWith('/api/jobs/1', expect.anything());
    expect(screen.getByRole('button', { name: t.jobs.deleteConfirm })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.jobs.deleteConfirm }));
    // useMutation's mutationFn runs in a microtask after mutate(), so the
    // fetch call lands asynchronously — wait for it rather than asserting
    // synchronously right after the click.
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith('/api/jobs/1', expect.objectContaining({ method: 'DELETE' })),
    );
  });

  it('surfaces the server error message on a 409', async () => {
    // The real backend answers job-action failures with http.Error, which is
    // plain text, not JSON (internal/observ/observ.go) — stub that shape
    // rather than JSON so this test actually exercises the text fallback.
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('job is importing\n', { status: 409 }))),
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'IMPORTING', status: 'active' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail({ state: 'IMPORTING' }));
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.delete }));
    fireEvent.click(screen.getByRole('button', { name: t.jobs.deleteConfirm }));

    await waitFor(() => expect(screen.getByText('job is importing')).toBeInTheDocument());
    expect(screen.queryByText(t.jobs.deleteFailed)).not.toBeInTheDocument();
  });

  it('disarms the delete confirm and falls back to the canned message when the server sends no body', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 500 }))));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    fireEvent.click(screen.getByRole('button', { name: t.jobs.delete }));
    fireEvent.click(screen.getByRole('button', { name: t.jobs.deleteConfirm }));

    await waitFor(() => expect(screen.getByText(t.jobs.deleteFailed)).toBeInTheDocument());
    // The confirm button reverted to its unarmed label instead of staying
    // primed next to the error.
    expect(screen.getByRole('button', { name: t.jobs.delete })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.jobs.deleteConfirm })).not.toBeInTheDocument();
  });
});

describe('meta row: nextAttemptAt and retries', () => {
  function stubFetchIndefinitely() {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  }

  it('shows nextAttemptAt and retries when both are present', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(
      queryKeys.jobs,
      [makeJob({ nextAttemptAt: '2026-01-01T12:00:00Z', retries: 2 })],
    );
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.getByText(t.jobs.nextAttempt(new Date('2026-01-01T12:00:00Z').toLocaleString('sv-SE')))).toBeInTheDocument();
    expect(screen.getByText(t.jobs.retries(2))).toBeInTheDocument();
  });

  it('hides nextAttemptAt and retries when unset', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ nextAttemptAt: '', retries: 0 })]);
    client.setQueryData(queryKeys.jobDetail(1), makeDetail());
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.queryByText(/Next attempt:/)).not.toBeInTheDocument();
    expect(screen.queryByText(/retries/)).not.toBeInTheDocument();
  });
});

describe('transfer live progress', () => {
  function stubFetchIndefinitely() {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  }

  function detailWithTransfers(transfers: TransferDetail[]): JobDetailDTO {
    return makeDetail({
      attempts: [
        {
          id: 100,
          username: 'peer-one',
          fileCount: transfers.length,
          state: 'ACTIVE',
          failReason: '',
          createdAt: '2026-01-01T00:00:00Z',
          updatedAt: '2026-01-01T00:00:00Z',
          transfers,
        },
      ],
    });
  }

  function makeTransfer(overrides: Partial<TransferDetail> = {}): TransferDetail {
    return {
      filename: '01.flac',
      state: 'IN_PROGRESS',
      bytesDone: 0,
      bytesTotal: 0,
      retries: 0,
      lastProgressAt: '',
      ...overrides,
    };
  }

  it('shows speed for a downloading transfer and queue position for a queued one', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(
      queryKeys.jobDetail(1),
      detailWithTransfers([
        makeTransfer({ filename: '01.flac', state: 'IN_PROGRESS', speed: 524288 }),
        makeTransfer({ filename: '02.flac', state: 'PENDING', queuePosition: 5 }),
      ]),
    );
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.getByText(/512 KB\/s/)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(t.jobs.queuePosition(5)))).toBeInTheDocument();
  });

  // A transfer waiting in the peer's queue moves no bytes, so its tick bar
  // must never flare — a flashing bar on an idle transfer would read as data
  // arriving when none is. Pins that guarantee via the DOM marker Ticks sets
  // on the one tick that's allowed to flare (mirrors the same assertion in
  // Jobs.test.tsx / JobExpansion.test.tsx for the other views, #198).
  it('flares the tick bar for a transferring file but not for a queued one', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(
      queryKeys.jobDetail(1),
      detailWithTransfers([
        makeTransfer({ filename: '01.flac', state: 'IN_PROGRESS', bytesDone: 100, bytesTotal: 200 }),
        makeTransfer({ filename: '02.flac', state: 'PENDING', queuePosition: 5, bytesDone: 0, bytesTotal: 200 }),
      ]),
    );
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    // Each transfer's filename span is a direct child of its own row div, so
    // the nearest ancestor <div> is exactly that row.
    const transferringRow = screen.getByText('01.flac').closest('div')!;
    const queuedRow = screen.getByText('02.flac').closest('div')!;
    expect(transferringRow.querySelectorAll('[data-flare="true"]')).toHaveLength(1);
    expect(queuedRow.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  it('omits speed and queue markers when the fields are absent', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.jobs, [makeJob({ state: 'DOWNLOADING', status: 'active' })]);
    client.setQueryData(
      queryKeys.jobDetail(1),
      detailWithTransfers([makeTransfer({ filename: '01.flac', state: 'ERRORED' })]),
    );
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);

    renderJobDetail('/jobs/1', client);

    expect(screen.getByText(/01\.flac/)).toBeInTheDocument();
    expect(screen.queryByText(/KB\/s|MB\/s/)).not.toBeInTheDocument();
    expect(screen.queryByText(/queue #/)).not.toBeInTheDocument();
  });
});

describe('placeholder-data guard', () => {
  // Small harness so navigating between job ids re-renders the *same*
  // JobDetail instance (as App.tsx's route does), which is what allows
  // TanStack Query's `placeholderData: keepPreviousData` to substitute the
  // previous job's data in the first place.
  function Harness() {
    const navigate = useNavigate();
    return (
      <>
        <button onClick={() => navigate('/jobs/2')}>go-to-2</button>
        <Routes>
          <Route path="/jobs/:id" element={<JobDetail />} />
        </Routes>
      </>
    );
  }

  it("shows loading, not job 1's stale attempts, while job 2's detail is still in flight", async () => {
    let resolveJob2Detail: (value: JobDetailDTO) => void = () => {};
    const job2DetailPromise = new Promise<JobDetailDTO>((resolve) => {
      resolveJob2Detail = resolve;
    });

    const job1Detail = makeDetail({
      id: 1,
      attempts: [
        {
          id: 100,
          username: 'peer-one',
          fileCount: 1,
          state: 'SUCCEEDED',
          failReason: '',
          createdAt: '2026-01-01T00:00:00Z',
          updatedAt: '2026-01-01T00:00:00Z',
          transfers: [],
        },
      ],
    });

    const jsonResponse = (body: unknown) =>
      Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));

    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/jobs') return jsonResponse([]);
        if (url === '/api/jobs/1/detail') return jsonResponse(job1Detail);
        if (url === '/api/jobs/1/events') return jsonResponse([]);
        if (url === '/api/jobs/2/detail') {
          return job2DetailPromise.then((data) => new Response(JSON.stringify(data), { status: 200 }));
        }
        if (url === '/api/jobs/2/events') return jsonResponse([]);
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );

    const client = new QueryClient({
      defaultOptions: { queries: { placeholderData: keepPreviousData, retry: false } },
    });

    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/jobs/1']}>
          <Harness />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await screen.findByText('peer-one');

    fireEvent.click(screen.getByRole('button', { name: 'go-to-2' }));

    // Job 2's detail fetch is still pending. Without the isPlaceholderData
    // guard, useJobDetail(2) would return job 1's data (isLoading: false,
    // isPlaceholderData: true) and the page would keep showing job 1's
    // attempt, silently mislabelled as job 2's.
    expect(screen.queryByText('peer-one')).not.toBeInTheDocument();
    expect(screen.getAllByText(t.query.loading).length).toBeGreaterThan(0);

    resolveJob2Detail(makeDetail({ id: 2, attempts: [] }));

    // EmptyState wraps the message in decorative dashes ("── … ──"), so match
    // by substring rather than the exact string (markup change, not content).
    await waitFor(() =>
      expect(screen.getByText(new RegExp(t.jobs.noAttempts))).toBeInTheDocument(),
    );
  });
});

describe('file ordering', () => {
  function transfer(filename: string): TransferDetail {
    return { filename, state: 'DONE', bytesDone: 1, bytesTotal: 1, retries: 0, lastProgressAt: '' };
  }

  it('lists files in track order regardless of the order the API returned them', () => {
    // The API hands transfers back in insertion order, which for a Soulseek
    // folder is arbitrary. A plain string sort would also put 10 before 2, so
    // this pins the numeric collation end to end, not just the comparator.
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, placeholderData: keepPreviousData } },
    });
    client.setQueryData(queryKeys.jobs, [makeJob()]);
    client.setQueryData(queryKeys.jobEvents(1), [] as JobEvent[]);
    client.setQueryData(
      queryKeys.jobDetail(1),
      makeDetail({
        attempts: [
          {
            id: 1,
            username: 'lossless_lars',
            fileCount: 3,
            state: 'ACTIVE',
            failReason: '',
            createdAt: '',
            updatedAt: '',
            transfers: [
              transfer('music\\10 Flamenco Sketches.flac'),
              transfer('music\\02 Freddie Freeloader.flac'),
              transfer('music\\01 So What.flac'),
            ],
          },
        ],
      }),
    );

    renderJobDetail('/jobs/1', client);

    const shown = screen
      .getAllByText(/\.flac$/)
      .map((el) => el.textContent);
    expect(shown).toEqual([
      '01 So What.flac',
      '02 Freddie Freeloader.flac',
      '10 Flamenco Sketches.flac',
    ]);
  });
});
