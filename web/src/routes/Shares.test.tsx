import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { SharesReport, UploadsReport } from '../api/types';
import { t } from '../strings';
import Shares from './Shares';

afterEach(() => vi.unstubAllGlobals());

function makeReport(overrides: Partial<SharesReport> = {}): SharesReport {
  return {
    enabled: true,
    scanning: false,
    indexedAt: '2026-07-20T14:32:05Z',
    scanDurationMs: 4200,
    directories: 3,
    files: 61443,
    totalBytes: 2.1 * 1024 * 1024 * 1024 * 1024,
    folders: [
      { name: 'Library', path: '/music/library', directories: 2, files: 48213, totalBytes: 742 * 1024 * 1024 * 1024 },
      { name: 'Incoming', path: '/music/incoming', directories: 1, files: 326, totalBytes: 18 * 1024 * 1024 * 1024 },
    ],
    ...overrides,
  };
}

function makeUploadsReport(overrides: Partial<UploadsReport> = {}): UploadsReport {
  return {
    enabled: false,
    slots: 0,
    active: 0,
    queued: 0,
    truncated: 0,
    uploads: [],
    ...overrides,
  };
}

// UploadsPanel mounts whenever Shares reaches its main (enabled) return, so
// every test that seeds an enabled SharesReport would otherwise let its
// useUploads() query attempt a real, unstubbed fetch. Seeding a disabled
// UploadsReport here by default keeps every existing test free of that
// network I/O; tests that actually exercise UploadsPanel override it below.
function newClient() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(queryKeys.uploads, makeUploadsReport());
  return client;
}

function renderShares(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Shares />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('loading state', () => {
  it('renders only the heading before data arrives', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    renderShares(newClient());
    expect(screen.getByRole('heading', { name: t.nav.shares })).toBeInTheDocument();
    expect(screen.queryByText(t.shares.disabledNotice)).not.toBeInTheDocument();
    expect(screen.queryByText(t.shares.emptyTitle)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.shares.rescan })).not.toBeInTheDocument();
  });
});

describe('disabled state', () => {
  it('shows a neutral notice and no rescan button or table', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport({ enabled: false, folders: [] }));
    renderShares(client);
    expect(screen.getByText(t.shares.disabledNotice)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.shares.rescan })).not.toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });
});

describe('no shares configured', () => {
  it('shows the warning card and the rescan button, but not the disabled notice', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport({ folders: [] }));
    renderShares(client);
    expect(screen.getByText(t.shares.emptyTitle)).toBeInTheDocument();
    expect(screen.getByText(t.shares.emptyBodyPrefix, { exact: false })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.shares.rescan })).toBeInTheDocument();
    expect(screen.queryByText(t.shares.disabledNotice)).not.toBeInTheDocument();
  });
});

describe('loaded state', () => {
  it('renders stat cards and the folder table', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    renderShares(client);
    expect(screen.getByText(t.shares.statFiles)).toBeInTheDocument();
    expect(screen.getByText('61443')).toBeInTheDocument();
    expect(screen.getByText('2.1 TB')).toBeInTheDocument();
    expect(screen.getByText('/music/library')).toBeInTheDocument();
    expect(screen.getByText('/music/incoming')).toBeInTheDocument();
    expect(screen.getByText('742.0 GB')).toBeInTheDocument();
  });

  it('shows "Never" when indexedAt is empty', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport({ indexedAt: '' }));
    renderShares(client);
    expect(screen.getAllByText(t.shares.statNever).length).toBeGreaterThan(0);
  });

  it('shows the scanning state and disables the button', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport({ scanning: true }));
    renderShares(client);
    expect(screen.getByRole('button', { name: t.shares.rescanning })).toBeDisabled();
  });
});

describe('rescan action', () => {
  it('starts a rescan on 202', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/shares/rescan') {
          return Promise.resolve(new Response(JSON.stringify({ ok: true, scanning: true }), { status: 202 }));
        }
        if (url === '/api/shares') {
          return Promise.resolve(new Response(JSON.stringify(makeReport({ scanning: true })), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    renderShares(client);
    fireEvent.click(screen.getByRole('button', { name: t.shares.rescan }));
    await waitFor(() => expect(screen.getByRole('button', { name: t.shares.rescanning })).toBeInTheDocument());
  });

  it('shows the conflict string on 409, not the generic failure string', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/shares/rescan') {
          return Promise.resolve(new Response(JSON.stringify({ error: 'a share scan is already in progress' }), { status: 409 }));
        }
        if (url === '/api/shares') {
          return Promise.resolve(new Response(JSON.stringify(makeReport()), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    renderShares(client);
    fireEvent.click(screen.getByRole('button', { name: t.shares.rescan }));
    await screen.findByText(t.shares.rescanConflict);
    expect(screen.queryByText(t.shares.rescanFailed)).not.toBeInTheDocument();
  });

  it('shows the unavailable string on 503, not the generic failure string', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/shares/rescan') {
          return Promise.resolve(new Response(JSON.stringify({ error: 'soulseek sharing is not enabled' }), { status: 503 }));
        }
        if (url === '/api/shares') {
          return Promise.resolve(new Response(JSON.stringify(makeReport()), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    renderShares(client);
    fireEvent.click(screen.getByRole('button', { name: t.shares.rescan }));
    await screen.findByText(t.shares.rescanUnavailable);
    expect(screen.queryByText(t.shares.rescanFailed)).not.toBeInTheDocument();
  });

  it('shows the generic failure string for a non-ApiError rejection', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/shares/rescan') {
          return Promise.reject(new TypeError('network error'));
        }
        if (url === '/api/shares') {
          return Promise.resolve(new Response(JSON.stringify(makeReport()), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    renderShares(client);
    fireEvent.click(screen.getByRole('button', { name: t.shares.rescan }));
    await screen.findByText(t.shares.rescanFailed);
  });
});

describe('uploads panel', () => {
  it('does not render when native Soulseek sharing is disabled', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport({ enabled: false, folders: [] }));
    client.setQueryData(
      queryKeys.uploads,
      makeUploadsReport({ enabled: true, slots: 2, active: 1, uploads: [
        { username: 'ripper_78', filename: 'Aphex Twin\\Windowlicker\\01.flac', active: true, position: 0, size: 1000, bytesWritten: 500 },
      ] }),
    );
    renderShares(client);
    expect(screen.queryByText(t.uploads.panelTitle)).not.toBeInTheDocument();
  });

  it('renders an active row with filename, peer, and byte counts', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    client.setQueryData(
      queryKeys.uploads,
      makeUploadsReport({
        enabled: true,
        slots: 2,
        active: 1,
        uploads: [
          {
            username: 'ripper_78',
            filename: 'Aphex Twin\\Windowlicker\\01 Windowlicker.flac',
            active: true,
            position: 0,
            size: 20 * 1024 * 1024,
            bytesWritten: 10 * 1024 * 1024,
          },
        ],
      }),
    );
    renderShares(client);
    expect(screen.getByText(t.uploads.panelTitle)).toBeInTheDocument();
    expect(screen.getByText(t.uploads.slotsInUse(1, 2))).toBeInTheDocument();
    expect(screen.getByText('01 Windowlicker.flac')).toBeInTheDocument();
    expect(screen.getByText('ripper_78')).toBeInTheDocument();
    expect(screen.getByText('10.0 MB / 20.0 MB')).toBeInTheDocument();
  });

  it('omits the byte caption for an active upload whose size is not resolved yet', () => {
    // dispatch marks a job active before runUpload stores the file size, so
    // size:0 is a real (brief) wire state. Rendering it would claim a 0-byte
    // file; the bar stays, only the caption is suppressed.
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    client.setQueryData(
      queryKeys.uploads,
      makeUploadsReport({
        enabled: true,
        slots: 2,
        active: 1,
        uploads: [
          {
            username: 'ripper_78',
            filename: 'Aphex Twin\\Windowlicker\\01 Windowlicker.flac',
            active: true,
            position: 0,
            size: 0,
            bytesWritten: 0,
          },
        ],
      }),
    );
    renderShares(client);
    expect(screen.getByText('01 Windowlicker.flac')).toBeInTheDocument();
    expect(screen.queryByText('0 MB / 0 MB')).not.toBeInTheDocument();
  });

  it('renders a queued row with its queue place instead of a progress bar', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    client.setQueryData(
      queryKeys.uploads,
      makeUploadsReport({
        enabled: true,
        slots: 2,
        active: 0,
        queued: 1,
        uploads: [
          { username: 'nordic_rip', filename: 'Burial\\Archangel.flac', active: false, position: 3, size: 0, bytesWritten: 0 },
        ],
      }),
    );
    renderShares(client);
    expect(screen.getByText('Archangel.flac')).toBeInTheDocument();
    expect(screen.getByText('nordic_rip')).toBeInTheDocument();
    expect(screen.getByText(t.uploads.queuePlace(3))).toBeInTheDocument();
  });

  it('shows the empty-state copy when there are no uploads', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    client.setQueryData(queryKeys.uploads, makeUploadsReport({ enabled: true, slots: 2 }));
    renderShares(client);
    expect(screen.getByText(t.uploads.empty)).toBeInTheDocument();
  });

  it('shows a truncation footer when entries were omitted', () => {
    const client = newClient();
    client.setQueryData(queryKeys.shares, makeReport());
    client.setQueryData(
      queryKeys.uploads,
      makeUploadsReport({
        enabled: true,
        slots: 2,
        active: 1,
        queued: 6,
        truncated: 5,
        uploads: [
          {
            username: 'ripper_78',
            filename: 'track.flac',
            active: true,
            position: 0,
            size: 1000,
            bytesWritten: 500,
          },
        ],
      }),
    );
    renderShares(client);
    expect(screen.getByText(t.uploads.truncated(5))).toBeInTheDocument();
  });
});
