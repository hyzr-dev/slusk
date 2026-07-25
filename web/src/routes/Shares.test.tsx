import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { SharesReport } from '../api/types';
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

function newClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
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
