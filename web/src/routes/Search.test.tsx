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

  // The placeholder alone yields an accessible name only as HTML-AAM's last
  // resort, and that name disappears the moment the user types — which is
  // exactly when a screen-reader user re-reads the field (WCAG 3.3.2). The
  // second assertion is the one that matters: it still holds with text in the
  // box.
  it('gives the query input an accessible name that survives typing', () => {
    renderSearch();
    const input = screen.getByRole('textbox', { name: t.search.queryLabel });
    fireEvent.change(input, { target: { value: 'radiohead' } });
    expect(screen.getByRole('textbox', { name: t.search.queryLabel })).toHaveValue('radiohead');
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
    expect(screen.getByText(t.search.resultsCount(1))).toBeInTheDocument();
  });

  // `parent` is the peer's parent DIRECTORY name, never a resolved artist —
  // a peer sharing /Music/Various Artists/In Rainbows/ would otherwise produce
  // a card asserting the artist is "Various Artists". Rendered bare in the
  // design's dim-text-after-title slot it reads as exactly the "Album —
  // Artist" idiom every music UI uses, so the DOM has to say what it is: a
  // hidden label for assistive tech, a trailing slash and the mono face for
  // everyone else.
  it('labels the parent folder as a folder rather than as an artist', async () => {
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
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'q' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));
    await screen.findByText('In Rainbows');

    // Never the bare artist-looking string on its own.
    expect(screen.queryByText('Radiohead')).not.toBeInTheDocument();
    expect(screen.getByText(t.search.folderLabel)).toBeInTheDocument();
    expect(screen.getByTitle(t.search.folderTitle('Radiohead'))).toHaveTextContent('Radiohead/');
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

describe('searching state', () => {
  // The state the "Asking peers on the network…" block was written for, and
  // the one it could never reach: nested inside `showResults`, and so behind
  // `groups.length > 0`, it could only appear once results had already
  // arrived — never while a search was running with nothing back yet.
  it('says it is working while a search is running with no results yet', async () => {
    const open = wireSession({ groups: [], total: 0, done: false, streaming: true });
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        return Promise.resolve(new Response(JSON.stringify(open), { status: 201 }));
      }
      if (/^\/api\/search\//.test(url)) {
        return Promise.resolve(new Response(JSON.stringify(open), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'obscure bootleg' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));

    expect(await screen.findByText(t.search.askingPeers)).toBeInTheDocument();
    // Not the no-hits state — the search has not finished, it just has
    // nothing yet, and those are different claims.
    expect(screen.queryByText(t.search.noHitsTitle('obscure bootleg'))).not.toBeInTheDocument();
  });

  // The gap between clicking Search and the 201 landing. `searchId` is still
  // the PREVIOUS search's id for the whole of that window, so without an
  // explicit guard the old results, their count and their header all stay on
  // screen looking current, with only an aria-hidden spinner to say otherwise.
  it('hides the previous search\'s results while the next start is in flight', async () => {
    const first = wireSession({ groups: [wireGroup({ id: 'g-old', title: 'Old Release' })], total: 1, done: true });
    // Held open, so the render is observed mid-POST rather than after it.
    let releaseSecondPost: (() => void) | undefined;
    const secondPosted = new Promise<void>((resolve) => { releaseSecondPost = resolve; });
    let posts = 0;

    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        posts += 1;
        if (posts === 1) return Promise.resolve(new Response(JSON.stringify(first), { status: 201 }));
        return secondPosted.then(() => new Response(JSON.stringify(wireSession({ id: 'second', groups: [], total: 0, done: false })), { status: 201 }));
      }
      if (/^\/api\/search\//.test(url)) {
        return Promise.resolve(new Response(JSON.stringify(first), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'first' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));
    expect(await screen.findByText('Old Release')).toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'second' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));

    // Mid-POST: the previous search's card and its result count are gone, and
    // the view says it is working instead.
    await waitFor(() => expect(screen.getByText(t.search.askingPeers)).toBeInTheDocument());
    expect(screen.queryByText('Old Release')).not.toBeInTheDocument();
    expect(screen.queryByText(t.search.resultsCount(1))).not.toBeInTheDocument();

    releaseSecondPost?.();
  });

  // A terminal contradiction: the session GET fails with nothing cached, so
  // `session` is undefined and `!session?.done` reads as "still running" — the
  // view rendered QueryNotice's failure AND "Asking peers on the network…" at
  // the same time, and nothing ever cleared it, because refetchInterval
  // returns false with no data so no further request is ever made.
  //
  // The cache being empty for a mounted id is not hypothetical: useStopSearch
  // removes exactly that key (it 404s once stopped), which is what this test
  // reproduces.
  it('stops claiming it is working once the session query fails with nothing cached', async () => {
    const running = wireSession({ done: false, streaming: true, groups: [], total: 0 });
    let sessionGetFails = false;
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        return Promise.resolve(new Response(JSON.stringify(running), { status: 201 }));
      }
      if (/^\/api\/search\//.test(url)) {
        if (sessionGetFails) return Promise.resolve(new Response('gone', { status: 404 }));
        return Promise.resolve(new Response(JSON.stringify(running), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    renderSearch(qc);
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'obscure bootleg' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));
    expect(await screen.findByText(t.search.askingPeers)).toBeInTheDocument();

    // What useStopSearch does to this key, plus the 404 that follows it.
    sessionGetFails = true;
    qc.removeQueries({ queryKey: ['search', running.id] });

    expect(await screen.findByText(t.query.failed)).toBeInTheDocument();
    expect(screen.queryByText(t.search.askingPeers)).not.toBeInTheDocument();
  });
});

describe('a failed start', () => {
  // `searchId` only advances in the 201's onSuccess, so on a FAILED start it
  // keeps pointing at the previous search. `starting` drops back to false and
  // the previous search's cards, count and header all reappear looking like
  // the answer to a query that never ran — and noHitsTitle(submittedQuery)
  // would name that query outright.
  it('does not resurrect the previous search\'s results when the next start fails', async () => {
    const first = wireSession({ groups: [wireGroup({ id: 'g-old', title: 'Old Release' })], total: 1, done: true });
    let posts = 0;
    vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/search' && init?.method === 'POST') {
        posts += 1;
        if (posts === 1) return Promise.resolve(new Response(JSON.stringify(first), { status: 201 }));
        return Promise.resolve(new Response('boom', { status: 500 }));
      }
      if (/^\/api\/search\//.test(url)) {
        return Promise.resolve(new Response(JSON.stringify(first), { status: 200 }));
      }
      return Promise.reject(new Error(`unexpected fetch ${url}`));
    }));

    renderSearch();
    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'first' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));
    expect(await screen.findByText('Old Release')).toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText(t.search.queryPlaceholder), { target: { value: 'second' } });
    fireEvent.click(screen.getByRole('button', { name: t.search.submit }));

    expect(await screen.findByText(t.search.startFailed)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('Old Release')).not.toBeInTheDocument());
    expect(screen.queryByText(t.search.resultsCount(1))).not.toBeInTheDocument();
    // And nothing claims the query that never ran came back empty.
    expect(screen.queryByText(t.search.noHitsTitle('second'))).not.toBeInTheDocument();
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

    // Queried by accessible name, not bare role: the select's only name comes
    // from the <label htmlFor> beside it, and a bare getByRole('combobox')
    // passes just as happily when that label is a plain <span> and a screen
    // reader announces "combobox, Best match" with no idea what it controls.
    fireEvent.change(screen.getByRole('combobox', { name: t.search.sortLabel }), { target: { value: 'size' } });
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
