import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Peer } from '../api/types';
import { t } from '../strings';
import Peers from './Peers';

function makePeer(overrides: Partial<Peer> = {}): Peer {
  return {
    username: 'peer',
    successCount: 0,
    failCount: 0,
    lastSuccessAt: '',
    lastFailAt: '',
    score: 0,
    artists: [],
    ...overrides,
  };
}

const peers: Peer[] = [
  makePeer({
    username: 'alice',
    score: 5,
    successCount: 10,
    failCount: 1,
    artists: [
      { artistId: 1, successCount: 2, failCount: 0, lastSuccessAt: '', lastFailAt: '', score: 1.5 },
    ],
  }),
  makePeer({ username: 'bob', score: 8, successCount: 2, failCount: 5 }),
  makePeer({ username: 'carol', score: 2, successCount: 20, failCount: 0 }),
];

function renderPeers() {
  const queryClient = new QueryClient();
  queryClient.setQueryData(queryKeys.peers, peers);
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
    expect(screen.getByText(t.peers.artistLine(1, '1.50', 2, 0))).toBeInTheDocument();
  });

  it('collapses the same peer on a second click', () => {
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText('alice'));
    expect(screen.queryByText(t.peers.artistLine(1, '1.50', 2, 0))).not.toBeInTheDocument();
  });

  it('only expands one peer at a time', () => {
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText('bob'));
    expect(screen.queryByText(t.peers.artistLine(1, '1.50', 2, 0))).not.toBeInTheDocument();
    expect(screen.getByText(t.peers.noArtistHistory)).toBeInTheDocument();
  });

  it('keeps a peer expanded across re-sorting, since expansion is keyed by username', () => {
    renderPeers();
    fireEvent.click(screen.getByText('alice'));
    fireEvent.click(screen.getByText(t.peers.gridHead.ok));
    expect(screen.getByText(t.peers.artistLine(1, '1.50', 2, 0))).toBeInTheDocument();
  });
});
