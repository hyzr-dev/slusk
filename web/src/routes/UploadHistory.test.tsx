import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { UploadHistoryEntry, UploadHistoryPage } from '../api/types';
import { t } from '../strings';
import UploadHistory from './UploadHistory';

afterEach(() => vi.unstubAllGlobals());

function makeEntry(overrides: Partial<UploadHistoryEntry> = {}): UploadHistoryEntry {
  return {
    id: 12,
    username: 'peer_nick',
    filename: 'Boards of Canada\\Geogaddi\\03 - Julie and Candy.flac',
    size: 41.2 * 1024 * 1024,
    bytesSent: 41.2 * 1024 * 1024,
    avgBytesPerSecond: 1.8 * 1024 * 1024,
    status: 'completed',
    detail: '',
    startedAt: '2026-07-31T14:20:00Z',
    finishedAt: '2026-07-31T14:22:00Z',
    ...overrides,
  };
}

// Same cache shape TanStack Query builds: each page's cursor is the previous
// page's oldest (= last, pages being newest-first) row id.
function seedHistory(client: QueryClient, pages: UploadHistoryPage[]) {
  client.setQueryData(queryKeys.uploadHistory, {
    pages,
    pageParams: pages.map((_, i) => (i === 0 ? 0 : pages[i - 1].uploads.at(-1)!.id)),
  });
}

function newClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function renderHistory(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <UploadHistory />
    </QueryClientProvider>,
  );
}

describe('UploadHistory rows', () => {
  it('renders a completed row with leaf filename, full path title, peer, timestamp, size and speed', () => {
    const client = newClient();
    seedHistory(client, [{ uploads: [makeEntry()], hasMore: false }]);
    renderHistory(client);

    const file = screen.getByText('03 - Julie and Candy.flac');
    expect(file).toHaveAttribute('title', 'Boards of Canada\\Geogaddi\\03 - Julie and Candy.flac');
    expect(screen.getByText(t.uploads.toPeerPrefix)).toBeInTheDocument();
    expect(screen.getByText('peer_nick')).toBeInTheDocument();
    expect(screen.getByText('41.2 MB')).toBeInTheDocument();
    expect(screen.getByText('1.8 MB/s')).toBeInTheDocument();
    expect(screen.getByText(t.uploads.historyStatus.completed)).toBeInTheDocument();
  });
});

describe('UploadHistory query states', () => {
  it('shows the empty state, and no load-older button, for an exhausted empty page', () => {
    const client = newClient();
    seedHistory(client, [{ uploads: [], hasMore: false }]);
    renderHistory(client);
    expect(screen.getByText(new RegExp(t.uploads.historyEmpty))).toBeInTheDocument();
    expect(screen.queryByText(t.uploads.historyLoadOlder)).not.toBeInTheDocument();
  });

  it('shows the loading notice, not the empty state, before the first fetch resolves', () => {
    stubFetchIndefinitely();
    renderHistory(newClient());
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
    expect(screen.queryByText(new RegExp(t.uploads.historyEmpty))).not.toBeInTheDocument();
  });
});

describe('UploadHistory load older', () => {
  it('does not render when hasMore is false', () => {
    const client = newClient();
    seedHistory(client, [{ uploads: [makeEntry()], hasMore: false }]);
    renderHistory(client);
    expect(screen.queryByText(t.uploads.historyLoadOlder)).not.toBeInTheDocument();
  });

  it('renders when hasMore is true with a non-empty page', () => {
    const client = newClient();
    seedHistory(client, [{ uploads: [makeEntry()], hasMore: true }]);
    renderHistory(client);
    expect(screen.getByText(t.uploads.historyLoadOlder)).toBeInTheDocument();
  });

  it('does not arm the button when an empty page still claims hasMore', () => {
    const client = newClient();
    seedHistory(client, [{ uploads: [], hasMore: true }]);
    renderHistory(client);
    expect(screen.queryByText(t.uploads.historyLoadOlder)).not.toBeInTheDocument();
  });

  it('pages backwards on click, using the previous page\'s last row id as the cursor', async () => {
    const client = newClient();
    seedHistory(client, [{ uploads: [makeEntry({ id: 12 })], hasMore: true }]);
    const fetchMock = vi.fn((url: string) => {
      expect(url).toBe('/api/uploads/history?before=12');
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify({ uploads: [makeEntry({ id: 5, filename: 'older.flac' })], hasMore: false })),
        json: () => Promise.resolve({ uploads: [makeEntry({ id: 5, filename: 'older.flac' })], hasMore: false }),
      } as Response);
    });
    vi.stubGlobal('fetch', fetchMock);
    renderHistory(client);

    fireEvent.click(screen.getByText(t.uploads.historyLoadOlder));
    await waitFor(() => expect(screen.getByText('older.flac')).toBeInTheDocument());
    expect(fetchMock).toHaveBeenCalledWith('/api/uploads/history?before=12');
  });
});

describe('UploadHistory transferred column', () => {
  it('shows a dash for a rejected row and never a zero measurement', () => {
    const client = newClient();
    seedHistory(client, [
      {
        uploads: [
          makeEntry({ status: 'rejected', bytesSent: 0, avgBytesPerSecond: 0, size: 5 * 1024 * 1024 }),
        ],
        hasMore: false,
      },
    ]);
    renderHistory(client);
    expect(screen.getAllByText('—')).toHaveLength(2);
    expect(screen.queryByText(/0 MB/)).not.toBeInTheDocument();
    expect(screen.queryByText(/0 KB\/s/)).not.toBeInTheDocument();
  });

  it('shows sent-of-total for an aborted row', () => {
    const client = newClient();
    seedHistory(client, [
      {
        uploads: [makeEntry({ status: 'aborted', bytesSent: 10 * 1024 * 1024, size: 40 * 1024 * 1024 })],
        hasMore: false,
      },
    ]);
    renderHistory(client);
    expect(screen.getByText('10.0 MB / 40.0 MB')).toBeInTheDocument();
  });
});

describe('UploadHistory detail line', () => {
  it('renders detail only when set', () => {
    const client = newClient();
    seedHistory(client, [
      {
        uploads: [
          makeEntry({ id: 1, detail: 'file unavailable' }),
          makeEntry({ id: 2, detail: '' }),
        ],
        hasMore: false,
      },
    ]);
    renderHistory(client);
    expect(screen.getByText('file unavailable')).toBeInTheDocument();
  });
});

describe('UploadHistory row identity', () => {
  it('renders duplicate (username, filename) rows differing only by id', () => {
    const client = newClient();
    seedHistory(client, [
      {
        uploads: [
          makeEntry({ id: 1, username: 'same_peer', filename: 'same.flac' }),
          makeEntry({ id: 2, username: 'same_peer', filename: 'same.flac' }),
        ],
        hasMore: false,
      },
    ]);
    renderHistory(client);
    expect(screen.getAllByText('same.flac')).toHaveLength(2);
  });
});
