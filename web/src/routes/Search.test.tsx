import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { WireSearchGroup, WireSearchSession } from '../api/types';
import { t } from '../strings';
import Search from './Search';

afterEach(() => vi.unstubAllGlobals());

function wireGroup(overrides: Partial<WireSearchGroup> = {}): WireSearchGroup {
  return {
    id: 'g1',
    peer: 'lossless_lars',
    folder: '@@abc/Music/Radiohead/In Rainbows',
    title: 'In Rainbows',
    parent: 'Radiohead',
    trackCount: 2,
    sizeBytes: 90000000,
    format: 'flac',
    freeUploadSlot: true,
    queueLength: 0,
    uploadSpeed: 940000,
    score: 0.9,
    files: [
      { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\01 - 15 Step.flac', name: '01 - 15 Step.flac', size: 30000000, durationSeconds: 237 },
      { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\02 - Bodysnatchers.flac', name: '02 - Bodysnatchers.flac', size: 32000000, durationSeconds: 242 },
    ],
    ...overrides,
  };
}

function wireSession(overrides: Partial<WireSearchSession> = {}): WireSearchSession {
  return {
    id: 'deadbeefdeadbeefdeadbeefdeadbeef',
    query: 'radiohead in rainbows',
    startedAt: '2026-01-01T00:00:00Z',
    done: true,
    streaming: true,
    total: 1,
    groups: [wireGroup()],
    ...overrides,
  };
}

function renderSearch(client?: QueryClient) {
  const qc = client ?? new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <Search />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('idle state', () => {
  it('shows the idle explanation and example query chips before any search runs', () => {
    renderSearch();
    expect(screen.getByText(t.search.idleTitle)).toBeInTheDocument();
    for (const example of t.search.examples) {
      expect(screen.getByRole('button', { name: example })).toBeInTheDocument();
    }
    expect(screen.queryByRole('list')).not.toBeInTheDocument();
  });
});

describe('starting a search and rendering results', () => {
  it('posts the query and renders a card per group from the 201 snapshot', async () => {
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        expect(JSON.parse(init.body as string)).toEqual({ query: 'radiohead in rainbows' });
        return Promise.resolve(new Response(JSON.stringify(wireSession()), { status: 201 }));
      }
      if (/^\/api\/search\//.test(url)) {
        return Promise.resolve(new Response(JSON.stringify(wireSession()), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'radiohead in rainbows' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));

    expect(await screen.findByText('In Rainbows')).toBeInTheDocument();
    expect(screen.getByText('Radiohead')).toBeInTheDocument();
    expect(screen.getByText(t.search.resultsCount(1))).toBeInTheDocument();
  });

  it('clicking an example chip runs that query immediately', async () => {
    const example = t.search.examples[0];
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        expect(JSON.parse(init.body as string)).toEqual({ query: example });
        return Promise.resolve(new Response(JSON.stringify(wireSession({ query: example })), { status: 201 }));
      }
      if (/^\/api\/search\//.test(url)) {
        return Promise.resolve(new Response(JSON.stringify(wireSession({ query: example })), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.click(screen.getByRole('button', { name: example }));

    expect(await screen.findByText('In Rainbows')).toBeInTheDocument();
  });
});

describe('no-hits state', () => {
  it('renders the no-hits message once a done session carries zero groups', async () => {
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        return Promise.resolve(
          new Response(JSON.stringify(wireSession({ groups: [], total: 0, done: true })), { status: 201 }),
        );
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'nothing matches this' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));

    expect(await screen.findByText(t.search.noHitsTitle('nothing matches this'))).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.search.newSearch })).toBeInTheDocument();
  });
});

// Shared setup for the results-view interaction tests below: drives the real
// search-and-render flow (type a query, submit, wait for the first card)
// rather than seeding the cache directly, so these tests exercise the same
// path a user does — the download tests in particular need a genuine
// mutation to fire against, which a pre-seeded cache alone wouldn't need.
// `extraFetch` handles any request beyond the two every search needs
// (POST /api/search and GET /api/search/{id}, both answering the same
// snapshot).
async function renderWithResults(
  groups: WireSearchGroup[] = [wireGroup()],
  total = groups.length,
  extraFetch?: (url: string, init?: RequestInit) => Promise<Response> | undefined,
) {
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const extra = extraFetch?.(url, init);
    if (extra) return extra;
    if (url === '/api/search' && init?.method === 'POST') {
      return Promise.resolve(new Response(JSON.stringify(wireSession({ groups, total })), { status: 201 }));
    }
    if (/^\/api\/search\//.test(url)) {
      return Promise.resolve(new Response(JSON.stringify(wireSession({ groups, total })), { status: 200 }));
    }
    return Promise.reject(new Error(`unexpected fetch ${url}`));
  }));
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  renderSearch(qc);
  fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'q' } });
  fireEvent.click(screen.getByRole('button', { name: t.search.submit }));
  await screen.findByText(groups[0].title);
  return qc;
}

describe('expansion', () => {
  it('shows the track list with checkboxes once a card header is expanded', async () => {
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        return Promise.resolve(new Response(JSON.stringify(wireSession()), { status: 201 }));
      }
      if (/^\/api\/search\//.test(url)) {
        return Promise.resolve(new Response(JSON.stringify(wireSession()), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'in rainbows' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));
    await screen.findByText('In Rainbows');

    fireEvent.click(screen.getByRole('button', { name: /In Rainbows/ }));

    expect(screen.getByText('01 - 15 Step.flac')).toBeInTheDocument();
    const checkboxes = screen.getAllByRole('checkbox');
    expect(checkboxes).toHaveLength(2);
    expect(checkboxes.every((box) => (box as HTMLInputElement).checked)).toBe(true);
  });
});

describe('sorting and format chips', () => {
  it('reorders cards when the sort select changes to Size', async () => {
    const small = wireGroup({ id: 'g-small', title: 'Small Release', sizeBytes: 1000, score: 0.95 });
    const big = wireGroup({ id: 'g-big', title: 'Big Release', sizeBytes: 9000000000, score: 0.1 });
    await renderWithResults([small, big], 2);

    const titles = () => screen.getAllByRole('button', { name: /Release/ }).map((el) => el.textContent);
    expect(titles()[0]).toContain('Small Release');

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'size' } });
    await waitFor(() => expect(titles()[0]).toContain('Big Release'));
  });

  it('filters cards to the active format chip', async () => {
    const flac = wireGroup({ id: 'g-flac', title: 'Flac Release', format: 'flac' });
    const mp3 = wireGroup({ id: 'g-mp3', title: 'Mp3 Release', format: 'mp3' });
    await renderWithResults([flac, mp3], 2);

    expect(screen.getByText('Mp3 Release')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'MP3' }));

    expect(screen.queryByText('Flac Release')).not.toBeInTheDocument();
    expect(screen.getByText('Mp3 Release')).toBeInTheDocument();
  });
});

describe('downloading', () => {
  it('posts the exact /api/jobs body for "Download album"', async () => {
    const group = wireGroup();
    let jobsBody: unknown;
    await renderWithResults([group], 1, (url, init) => {
      if (url === '/api/jobs' && init?.method === 'POST') {
        jobsBody = JSON.parse(init.body as string);
        return Promise.resolve(new Response(JSON.stringify({ id: 42 }), { status: 201 }));
      }
      return undefined;
    });

    fireEvent.click(screen.getByRole('button', { name: t.search.downloadAlbum }));

    await waitFor(() => expect(jobsBody).toEqual({
      title: 'In Rainbows',
      artist: 'Radiohead',
      peer: 'lossless_lars',
      files: [
        { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\01 - 15 Step.flac', size: 30000000 },
        { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\02 - Bodysnatchers.flac', size: 32000000 },
      ],
    }));
    expect(await screen.findByText(t.search.queuedNotice)).toBeInTheDocument();
  });

  it('shows the busy message inline on the card when POST /api/jobs answers 409', async () => {
    const group = wireGroup();
    await renderWithResults([group], 1, (url, init) => {
      if (url === '/api/jobs' && init?.method === 'POST') {
        return Promise.resolve(
          new Response(JSON.stringify({ error: 'remote file already claimed by another live candidate' }), { status: 409 }),
        );
      }
      return undefined;
    });

    fireEvent.click(screen.getByRole('button', { name: t.search.downloadAlbum }));

    expect(await screen.findByText(t.search.busyNotice)).toBeInTheDocument();
  });
});
