import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Peer, PeerArtist, PeerHistory } from '../api/types';
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

function renderPeers() {
  // A real refetch on mount would otherwise hit the unmocked global fetch;
  // keep it pending indefinitely so the seeded data is what's asserted on.
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(queryKeys.peers, peers);
  queryClient.setQueryData(queryKeys.peerHistory('alice'), aliceHistory);
  queryClient.setQueryData(queryKeys.peerHistory('bob'), { username: 'bob', artists: [] });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Peers />
      </MemoryRouter>
    </QueryClientProvider>,
  );
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
  it('defaults to score, descending', () => {
    renderPeers();
    expect(bodyUsernames()).toEqual(['bob', 'alice', 'carol']);
  });

  it('toggles direction when the active column is clicked again', () => {
    renderPeers();
    fireEvent.click(screen.getByText(t.peers.gridHead.score));
    expect(bodyUsernames()).toEqual(['carol', 'alice', 'bob']);
  });

  it('switches to a new column and always starts descending, regardless of prior direction', () => {
    renderPeers();
    // First flip score to ascending, so a naive "remember last direction"
    // implementation would carry ascending over to the new column.
    fireEvent.click(screen.getByText(t.peers.gridHead.score));
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    expect(bodyUsernames()).toEqual(['carol', 'alice', 'bob']);
  });

  it('sorts non-mutating, leaving the underlying query data untouched', () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(queryKeys.peers, peers);
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Peers />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    expect(peers.map((p) => p.username)).toEqual(['alice', 'bob', 'carol']);
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
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    expect(screen.getByText(aliceArtistLine)).toBeInTheDocument();
  });

  // The id fallback, not a placeholder name: album_jobs.artist_name defaults
  // to '' and an artist whose jobs are all gone has no row at all, so ''
  // means "no name known" and the row must still say which artist it is.
  it('labels an artist with no known name by its id', () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.peers, peers);
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
    client.setQueryData(queryKeys.peers, peers);
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
    client.setQueryData(queryKeys.peers, [makePeer({ username: 'awkward peer/name' })]);
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
