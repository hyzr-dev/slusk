import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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

// Finds the caret toggle button for a row by its accessible name — both the
// "show" and "hide" labels are valid names depending on expanded state, so
// this tries both rather than hardcoding one.
function caretFor(filename: string): HTMLElement {
  const row = screen.getByText(filename).closest('[role="row"]');
  if (!row) throw new Error(`no row found for ${filename}`);
  return within(row as HTMLElement).getByRole('button');
}

describe('UploadHistory rows', () => {
  it('shows state, leaf filename with full path in title, peer and avg speed on the row', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [{ uploads: [makeEntry()], hasMore: false }]);
    renderHistory(client);

    const file = screen.getByText('03 - Julie and Candy.flac');
    expect(file).toHaveAttribute('title', 'Boards of Canada\\Geogaddi\\03 - Julie and Candy.flac');
    expect(screen.getByText(t.uploads.toPeerPrefix)).toBeInTheDocument();
    expect(screen.getByText('peer_nick')).toBeInTheDocument();
    expect(screen.getByText('1.8 MB/s')).toBeInTheDocument();
    expect(screen.getByText(t.uploads.historyStatus.completed)).toBeInTheDocument();
  });

  it('does not render size, timestamp or detail until the row is expanded', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [{ uploads: [makeEntry({ detail: 'file unavailable' })], hasMore: false }]);
    const { container } = renderHistory(client);

    // Node absence, not just text absence — an unconditionally-rendered but
    // visually-hidden element would still satisfy a bare queryByText check.
    expect(screen.queryByText('41.2 MB')).not.toBeInTheDocument();
    expect(screen.queryByText(/2026-07-31/)).not.toBeInTheDocument();
    expect(screen.queryByText('file unavailable')).not.toBeInTheDocument();
    expect(container.querySelectorAll('[id^="upload-history-expansion-"]')).toHaveLength(0);
  });

  it('expands on caret click to reveal size, finished timestamp and detail, and collapses again on a second click', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [{ uploads: [makeEntry({ detail: 'file unavailable' })], hasMore: false }]);
    renderHistory(client);

    const caret = caretFor('03 - Julie and Candy.flac');
    expect(caret).toHaveAttribute('aria-expanded', 'false');
    expect(caret).toHaveAccessibleName(t.uploads.historyShowDetails);

    fireEvent.click(caret);
    expect(caret).toHaveAttribute('aria-expanded', 'true');
    expect(caret).toHaveAccessibleName(t.uploads.historyHideDetails);
    expect(screen.getByText('41.2 MB')).toBeInTheDocument();
    expect(screen.getByText(t.uploads.historyFinishedLabel)).toBeInTheDocument();
    expect(screen.getByText('file unavailable')).toBeInTheDocument();

    fireEvent.click(caret);
    expect(caret).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('41.2 MB')).not.toBeInTheDocument();
    expect(screen.queryByText('file unavailable')).not.toBeInTheDocument();
  });

  it('keeps only one row expanded at a time', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [
      {
        uploads: [
          makeEntry({ id: 1, filename: 'first.flac' }),
          makeEntry({ id: 2, filename: 'second.flac' }),
        ],
        hasMore: false,
      },
    ]);
    renderHistory(client);

    fireEvent.click(caretFor('first.flac'));
    expect(caretFor('first.flac')).toHaveAttribute('aria-expanded', 'true');
    expect(caretFor('second.flac')).toHaveAttribute('aria-expanded', 'false');

    fireEvent.click(caretFor('second.flac'));
    expect(caretFor('first.flac')).toHaveAttribute('aria-expanded', 'false');
    expect(caretFor('second.flac')).toHaveAttribute('aria-expanded', 'true');
  });
});

describe('UploadHistory query states', () => {
  it('shows the empty state, and no load-older button, for an exhausted empty page', () => {
    const client = newClient();
    stubFetchIndefinitely();
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
    stubFetchIndefinitely();
    seedHistory(client, [{ uploads: [makeEntry()], hasMore: false }]);
    renderHistory(client);
    expect(screen.queryByText(t.uploads.historyLoadOlder)).not.toBeInTheDocument();
  });

  it('renders when hasMore is true with a non-empty page', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [{ uploads: [makeEntry()], hasMore: true }]);
    renderHistory(client);
    expect(screen.getByText(t.uploads.historyLoadOlder)).toBeInTheDocument();
  });

  it('does not arm the button when an empty page still claims hasMore', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [{ uploads: [], hasMore: true }]);
    renderHistory(client);
    expect(screen.queryByText(t.uploads.historyLoadOlder)).not.toBeInTheDocument();
  });

  // The seeded page carries two rows (ids 12 and 8, newest-first) specifically
  // so this test can tell "the previous page's FIRST row id" apart from "the
  // previous page's LAST row id" — with a single row those two are the same
  // value and the test cannot pin the cursor choice at all.
  it('pages backwards on click, using the previous page\'s last row id as the cursor', async () => {
    const client = newClient();
    seedHistory(client, [
      { uploads: [makeEntry({ id: 12 }), makeEntry({ id: 8, filename: 'second.flac' })], hasMore: true },
    ]);
    const fetchMock = vi.fn((url: string) => {
      expect(url).toBe('/api/uploads/history?before=8');
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
    expect(fetchMock).toHaveBeenCalledWith('/api/uploads/history?before=8');
  });
});

describe('UploadHistory transferred column', () => {
  it('shows a dash for a rejected row\'s size when expanded, and a dash for speed on the row — never a zero measurement', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [
      {
        uploads: [
          makeEntry({ status: 'rejected', bytesSent: 0, avgBytesPerSecond: 0, size: 5 * 1024 * 1024 }),
        ],
        hasMore: false,
      },
    ]);
    renderHistory(client);

    // Speed lives on the row and must already read '—'.
    expect(screen.getByText('—')).toBeInTheDocument();

    fireEvent.click(caretFor('03 - Julie and Candy.flac'));
    expect(screen.getAllByText('—')).toHaveLength(2); // row speed + expansion size
    expect(screen.queryByText(/0 MB/)).not.toBeInTheDocument();
    expect(screen.queryByText(/0 KB\/s/)).not.toBeInTheDocument();
  });

  it('shows sent-of-total for an aborted row once expanded', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [
      {
        uploads: [makeEntry({ status: 'aborted', bytesSent: 10 * 1024 * 1024, size: 40 * 1024 * 1024 })],
        hasMore: false,
      },
    ]);
    renderHistory(client);
    fireEvent.click(caretFor('03 - Julie and Candy.flac'));
    expect(screen.getByText('10.0 MB / 40.0 MB')).toBeInTheDocument();
  });
});

describe('UploadHistory detail line', () => {
  it('renders detail only when set, and only once expanded', () => {
    const client = newClient();
    stubFetchIndefinitely();
    seedHistory(client, [
      {
        uploads: [
          makeEntry({ id: 1, filename: 'has-detail.flac', detail: 'file unavailable' }),
          makeEntry({ id: 2, filename: 'no-detail.flac', detail: '' }),
        ],
        hasMore: false,
      },
    ]);
    const { container } = renderHistory(client);

    // Expand the EMPTY-detail row on its own first: only one row is expanded
    // at a time (see the row-identity describe block below), so this is the
    // only way to observe its expansion in isolation rather than always
    // looking at whichever row happens to be open last.
    fireEvent.click(caretFor('no-detail.flac'));
    expect(screen.queryByText('file unavailable')).not.toBeInTheDocument();
    // Asserting only that the text is absent would survive an unconditional
    // `<div className={styles.historyDetail}>{e.detail}</div>` too — an empty
    // string renders no text node either way. Pin the node's absence itself.
    expect(container.querySelectorAll('[class*="historyDetail"]')).toHaveLength(0);

    fireEvent.click(caretFor('has-detail.flac'));
    expect(screen.getByText('file unavailable')).toBeInTheDocument();
    expect(container.querySelectorAll('[class*="historyDetail"]')).toHaveLength(1);
  });
});

describe('UploadHistory row identity', () => {
  // Two entries share (username, filename) but have distinct ids. React only
  // warns about "two children with the same key" when the *keys themselves*
  // collide — since id is unique here, key={e.id} produces no warning, while
  // key={`${e.username}-${e.filename}`} would collide and warn. That makes
  // the console.error spy an honest pin of the id-as-key decision: swapping
  // the key back to the composite string flips this from silent to warning.
  it('keys rows by id rather than by (username, filename), so React never warns about a duplicate key', () => {
    const client = newClient();
    stubFetchIndefinitely();
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
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
    const sameKeyWarning = consoleError.mock.calls.some((args) =>
      args.some((a) => typeof a === 'string' && a.includes('same key')),
    );
    expect(sameKeyWarning).toBe(false);
    consoleError.mockRestore();
  });
});
