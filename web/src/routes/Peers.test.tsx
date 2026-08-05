import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DEFAULT_PEER_PAGE_PARAMS, PEERS_PAGE_SIZE, queryKeys } from '../api/queries';
import type { Peer, PeerArtist, PeerHistory, PeerPageParams } from '../api/types';
import { t } from '../strings';
import Peers from './Peers';

afterEach(() => vi.unstubAllGlobals());

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function stubFetchFailing() {
  vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('boom'))));
}

function makePeer(overrides: Partial<Peer> = {}): Peer {
  return {
    username: 'peer',
    successCount: 0,
    failCount: 0,
    lastSuccessAt: '',
    lastFailAt: '',
    score: 0,
    ...overrides,
  };
}

function makeArtist(overrides: Partial<PeerArtist> = {}): PeerArtist {
  return {
    artistId: 1,
    artistName: '',
    successCount: 0,
    failCount: 0,
    lastSuccessAt: '',
    lastFailAt: '',
    score: 0,
    ...overrides,
  };
}

const peers: Peer[] = [
  makePeer({ username: 'alice', score: 5, successCount: 10, failCount: 1 }),
  makePeer({ username: 'bob', score: 8, successCount: 2, failCount: 5 }),
  makePeer({ username: 'carol', score: 2, successCount: 20, failCount: 0 }),
];

// alice's artist history, which since #424 lives behind its own request rather
// than inside the list response. Seeded into the cache under the same key
// usePeerHistory reads, so expansion tests never depend on a stubbed fetch
// resolving.
const aliceHistory: PeerHistory = {
  username: 'alice',
  artists: [makeArtist({ artistId: 1, artistName: 'Named Artist', successCount: 2, score: 1.5 })],
};
const aliceArtistLine = t.peers.artistLine('Named Artist', '1.50', 2, 0);

// Every page the component might ask for has to be seeded separately now that
// sort and page are part of the query key: a re-sort is a different request,
// not a client-side reshuffle of data already in hand (issue #426).
function seedPage(
  client: QueryClient,
  rows: Peer[],
  params: Partial<PeerPageParams> = {},
  total = rows.length,
) {
  client.setQueryData(queryKeys.peersPage({ ...DEFAULT_PEER_PAGE_PARAMS, ...params }), {
    peers: rows,
    total,
  });
}

function renderWith(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Peers />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// total defaults to the seeded row count; pass a bigger one to get a pager
// with more than a single page in it.
function renderPeers(total = peers.length) {
  // A real refetch on mount would otherwise hit the unmocked global fetch;
  // keep it pending indefinitely so the seeded data is what's asserted on.
  const fetchMock = vi.fn(() => new Promise(() => {}));
  vi.stubGlobal('fetch', fetchMock);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  seedPage(queryClient, peers, {}, total);
  queryClient.setQueryData(queryKeys.peerHistory('alice'), aliceHistory);
  queryClient.setQueryData(queryKeys.peerHistory('bob'), { username: 'bob', artists: [] });
  renderWith(queryClient);
  return { queryClient, fetchMock };
}

// The list URLs the component has asked for, in order. Sorting and paging are
// server-side, so "did the order change" is a question about requests now, not
// about the rendered rows — asserting on row order would only re-test the
// seeded fixture.
function listRequests(fetchMock: { mock: { calls: unknown[][] } }): string[] {
  return fetchMock.mock.calls
    .map(([url]) => String(url))
    .filter((url) => url.startsWith('/api/peers?'));
}

// Every username, in document order — the cheapest way to read back the
// current sort order now that rows are grid divs carrying an ARIA row role
// rather than a native <table>. Each username is a unique text node, and
// testing-library's getAllByText returns matches in DOM order, so this needs
// no assumption about row structure.
function bodyUsernames() {
  return screen.getAllByText(/^(alice|bob|carol)$/).map((el) => el.textContent);
}

describe('Peers sorting', () => {
  it('asks the server for score, descending, by default', () => {
    const { fetchMock } = renderPeers();
    // Seeded, so the only request is the mount refetch — which is exactly the
    // URL under test.
    expect(listRequests(fetchMock)).toContain('/api/peers?page=0&sort=score&dir=desc');
  });

  it('renders the server order as given, without re-sorting it', () => {
    renderPeers();
    // `peers` is in username order and deliberately NOT in score order: the
    // server's order is the one on screen, so a client-side sort creeping back
    // in would fail here.
    expect(bodyUsernames()).toEqual(['alice', 'bob', 'carol']);
  });

  it('toggles direction when the active column is clicked again', () => {
    const { fetchMock } = renderPeers();
    fireEvent.click(screen.getByText(t.peers.gridHead.score));
    expect(listRequests(fetchMock)).toContain('/api/peers?page=0&sort=score&dir=asc');
  });

  it('switches to a new column and always starts descending, regardless of prior direction', () => {
    const { fetchMock } = renderPeers();
    // First flip score to ascending, so a naive "remember last direction"
    // implementation would carry ascending over to the new column.
    fireEvent.click(screen.getByText(t.peers.gridHead.score));
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    expect(listRequests(fetchMock)).toContain('/api/peers?page=0&sort=successCount&dir=desc');
  });

  it('sorts by username when the PEER column is clicked', () => {
    const { fetchMock } = renderPeers();
    fireEvent.click(screen.getByText(t.peers.gridHead.peer));
    expect(listRequests(fetchMock)).toContain('/api/peers?page=0&sort=username&dir=desc');
  });

  it('returns to the first page when the sort changes', () => {
    const { fetchMock } = renderPeers(97);
    fireEvent.click(screen.getByLabelText(t.pager.pageLabel(2)));
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    // Offset 25 under one order names a different peer than offset 25 under
    // another, so staying on page 2 would land the user nowhere in particular.
    expect(listRequests(fetchMock)).toContain('/api/peers?page=0&sort=successCount&dir=desc');
  });

  it('exposes the grid as an ARIA table with a sortable, announced sort column', () => {
    renderPeers();
    const table = screen.getByRole('table');
    // Five columns, seeded rows — the count would silently drop if a cell
    // span went missing, since nothing else in this suite asserts it exists.
    expect(within(table).getAllByRole('cell')).toHaveLength(peers.length * 5);
    const scoreHeader = within(table).getByRole('columnheader', { name: t.peers.gridHead.score });
    expect(scoreHeader).toHaveAttribute('aria-sort', 'descending');
    const okHeader = within(table).getByRole('columnheader', { name: t.peers.gridHead.ok });
    expect(okHeader).toHaveAttribute('aria-sort', 'none');

    fireEvent.click(screen.getByText(t.peers.gridHead.score));
    expect(scoreHeader).toHaveAttribute('aria-sort', 'ascending');
  });

  // LAST SEEN is derived client-side from two independent timestamps and the
  // backend ranks by neither, so offering to sort by it would promise an order
  // over the whole set that only the fetched page could ever honour.
  it('does not offer LAST SEEN as a sort column', () => {
    renderPeers();
    const header = screen.getByRole('columnheader', { name: t.peers.gridHead.lastSeen });
    expect(within(header).queryByRole('button')).not.toBeInTheDocument();
  });
});

describe('Peers paging', () => {
  function renderPaged(total: number) {
    const fetchMock = vi.fn(() => new Promise(() => {}));
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, peers, {}, total);
    renderWith(client);
    return fetchMock;
  }

  it('reports the range and the whole-set total, not the page length', () => {
    renderPaged(97);
    expect(screen.getByText(t.peers.resultRange(1, PEERS_PAGE_SIZE, 97))).toBeInTheDocument();
  });

  it('requests the page the pager was pointed at', () => {
    const fetchMock = renderPaged(97);
    fireEvent.click(screen.getByLabelText(t.pager.pageLabel(3)));
    expect(listRequests(fetchMock)).toContain('/api/peers?page=2&sort=score&dir=desc');
  });

  // This view binds no keys, so its pager may not advertise any (#434).
  it('shows no keyboard hint on the prev/next buttons', () => {
    renderPaged(97);
    expect(screen.getByRole('button', { name: t.pager.previousPage })).toHaveTextContent(/^PREV$/);
    expect(screen.getByRole('button', { name: t.pager.nextPage })).toHaveTextContent(/^NEXT$/);
  });

  it('offers as many pages as the total implies, not as many as the page holds', () => {
    renderPaged(97);
    // 97 peers at 25 a page is four pages; a pager sized from the three seeded
    // rows would offer one.
    expect(screen.getByLabelText(t.pager.pageLabel(4))).toBeInTheDocument();
    expect(screen.queryByLabelText(t.pager.pageLabel(5))).not.toBeInTheDocument();
  });

  // A pager offering the page you are already on is a control with nothing to
  // do. Most instances know a handful of peers, so this is the common case,
  // not an edge one — and the result range goes with it: the count is the row
  // count, visible in full a few pixels above.
  it('shows no pager at all when the whole set fits on one page', () => {
    renderPaged(peers.length);
    expect(screen.queryByLabelText(t.pager.pageLabel(1))).not.toBeInTheDocument();
    expect(screen.queryByText(t.pager.nextPage)).not.toBeInTheDocument();
    expect(
      screen.queryByText(t.peers.resultRange(1, peers.length, peers.length)),
    ).not.toBeInTheDocument();
  });

  it('renders an out-of-range page as empty while still reporting the real total', () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    seedPage(client, [], {}, 97);
    renderWith(client);
    // Not t.peers.empty: 97 peers exist, none of them here. Claiming the list
    // is empty beside a pager reading "of 97 peers" would be a contradiction.
    expect(screen.getByText(t.peers.pastTheEnd, { exact: false })).toBeInTheDocument();
    expect(screen.queryByText(t.peers.empty, { exact: false })).not.toBeInTheDocument();
    expect(screen.getByLabelText(t.pager.pageLabel(4))).toBeInTheDocument();
  });
});

describe('Peers row expansion', () => {
  it('expands a peer to show its per-artist history', () => {
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    expect(screen.getByText(aliceArtistLine)).toBeInTheDocument();
  });

  it('collapses the same peer on a second click', () => {
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText('alice'));
    expect(screen.queryByText(aliceArtistLine)).not.toBeInTheDocument();
  });

  it('only expands one peer at a time', () => {
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText('bob'));
    expect(screen.queryByText(aliceArtistLine)).not.toBeInTheDocument();
    expect(screen.getByText(t.peers.noArtistHistory)).toBeInTheDocument();
  });

  it('keeps a peer expanded across re-sorting, since expansion is keyed by username', () => {
    const { queryClient } = renderPeers();
    // The re-sorted page has to be seeded too: sorting is a fresh request
    // since #426, and an unseeded key would blank the list rather than
    // reordering it, so nothing would be left to stay expanded.
    seedPage(queryClient, [...peers].reverse(), { sort: 'successCount' });
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    expect(screen.getByText(aliceArtistLine)).toBeInTheDocument();
  });

  // Deliberately the same username on both pages, which cannot happen for real:
  // it is the only way to make this assertion non-vacuous. An expansion keyed
  // by username survives a page change silently whenever the name is gone from
  // the new page, so seeding a disjoint page would pass without the reset.
  it('collapses the expanded peer when the page changes', () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    seedPage(client, peers, {}, 97);
    seedPage(client, peers, { page: 1 }, 97);
    client.setQueryData(queryKeys.peerHistory('alice'), aliceHistory);
    renderWith(client);

    fireEvent.click(screen.getByText('alice'));
    expect(screen.getByText(aliceArtistLine)).toBeInTheDocument();

    fireEvent.click(screen.getByLabelText(t.pager.pageLabel(2)));
    expect(screen.queryByText(aliceArtistLine)).not.toBeInTheDocument();
  });

  // The id fallback, not a placeholder name: album_jobs.artist_name defaults
  // to '' and an artist whose jobs are all gone has no row at all, so ''
  // means "no name known" and the row must still say which artist it is.
  it('labels an artist with no known name by its id', () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, peers);
    client.setQueryData(queryKeys.peerHistory('alice'), {
      username: 'alice',
      artists: [makeArtist({ artistId: 7, artistName: '', successCount: 2, score: 1.5 })],
    });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Peers />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByText('alice'));
    expect(screen.getByText(t.peers.artistLine('Artist #7', '1.50', 2, 0))).toBeInTheDocument();
  });
});

// The expansion became a request of its own in #424, so it has states the
// old in-hand render could not: still fetching, and failed.
describe('Peers expansion query state', () => {
  function renderWithoutHistory() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, peers);
    return render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Peers />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it('shows a loading line while the expanded peer’s history is in flight', () => {
    stubFetchIndefinitely();
    renderWithoutHistory();
    fireEvent.click(screen.getByText('alice'));
    expect(screen.getByText(t.peers.artistHistoryLoading)).toBeInTheDocument();
    // Not "no artist history": nothing has been established yet, and saying
    // so would be a claim the interface cannot back.
    expect(screen.queryByText(t.peers.noArtistHistory)).not.toBeInTheDocument();
  });

  it('shows a failure line when the expanded peer’s history cannot be fetched', async () => {
    stubFetchFailing();
    renderWithoutHistory();
    fireEvent.click(screen.getByText('alice'));
    expect(await screen.findByText(t.peers.artistHistoryFailed)).toBeInTheDocument();
    expect(screen.queryByText(t.peers.noArtistHistory)).not.toBeInTheDocument();
  });

  it('does not fetch any history until a row is expanded', () => {
    const fetchMock = vi.fn((url: string) => {
      void url;
      return new Promise(() => {});
    });
    vi.stubGlobal('fetch', fetchMock);
    renderWithoutHistory();
    expect(fetchMock.mock.calls.some(([url]) => url.startsWith('/api/peers/'))).toBe(false);
  });

  it('requests the expanded peer by URL-encoded username', () => {
    const fetchMock = vi.fn(() => new Promise(() => {}));
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    seedPage(client, [makePeer({ username: 'awkward peer/name' })]);
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Peers />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByText('awkward peer/name'));
    expect(fetchMock).toHaveBeenCalledWith('/api/peers/awkward%20peer%2Fname');
  });
});

describe('Peers query state', () => {
  it('shows the loading line, not the empty message, before the first fetch resolves', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Peers />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
    expect(screen.queryByText(t.peers.empty, { exact: false })).not.toBeInTheDocument();
    expect(screen.getByText(t.peers.gridHead.peer)).toBeInTheDocument();
  });

  it('shows the failed line, not the empty message, when the fetch never succeeds', async () => {
    stubFetchFailing();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Peers />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByText(t.query.failed)).toBeInTheDocument();
    expect(screen.queryByText(t.peers.empty, { exact: false })).not.toBeInTheDocument();
    expect(screen.getByText(t.peers.gridHead.peer)).toBeInTheDocument();
  });
});
