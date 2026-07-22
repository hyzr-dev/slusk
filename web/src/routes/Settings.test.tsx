import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { AppConfig } from '../api/types';
import { t } from '../strings';
import Settings from './Settings';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function makeConfig(overrides: Partial<AppConfig> = {}): AppConfig {
  return {
    lidarrUrl: 'http://lidarr:8686',
    lidarrApiKeyConfigured: true,
    wantedSyncInterval: '5m',
    maxActive: 3,
    minBitrate: 192,
    stallTimeout: '5m',
    soulseekEnabled: false,
    writable: true,
    ...overrides,
  };
}

function newClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function renderSettings(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Settings />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('connection tests', () => {
  it('starts untested and shows Connected after a successful test', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/config/test/lidarr') {
          return Promise.resolve(new Response(JSON.stringify({ ok: true }), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    expect(screen.getByText(t.settings.testStatus.untested)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.settings.testConnection }));
    await screen.findByText(t.settings.testStatus.success);
  });

  it('shows the failure reason when the test reports ok:false', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/config/test/lidarr') {
          return Promise.resolve(
            new Response(
              JSON.stringify({ ok: false, error: 'lidarr rejected the API key (status 401)' }),
              { status: 200 },
            ),
          );
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.testConnection }));
    await screen.findByText(t.settings.testStatus.failure);
    expect(screen.getByText(/rejected the API key/)).toBeInTheDocument();
  });

  it('marks the test failed when the endpoint itself is unreachable', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/config/test/lidarr') {
          return Promise.resolve(new Response('nope', { status: 502 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.testConnection }));
    await screen.findByText(t.settings.testStatus.failure);
    expect(screen.getByText(t.settings.testUnreachable)).toBeInTheDocument();
  });

  it('offers the Soulseek test only when soulseek is enabled', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));

    const disabled = newClient();
    disabled.setQueryData(queryKeys.config, makeConfig({ soulseekEnabled: false }));
    const { unmount } = renderSettings(disabled);
    expect(screen.queryByText(t.settings.soulseek)).not.toBeInTheDocument();
    unmount();

    const enabled = newClient();
    enabled.setQueryData(queryKeys.config, makeConfig({ soulseekEnabled: true }));
    renderSettings(enabled);
    expect(screen.getByText(t.settings.soulseek)).toBeInTheDocument();
  });
});

describe('editable settings', () => {
  it('renders enabled, seeded inputs when the config is writable', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    expect(screen.getByDisplayValue('http://lidarr:8686')).not.toBeDisabled();
    expect(screen.getByRole('button', { name: t.settings.save })).toBeInTheDocument();
    expect(screen.queryByText(t.settings.notWritableNotice)).not.toBeInTheDocument();
    // The API key input never shows a stored value, only a placeholder hint.
    const apiKeyInput = screen.getByPlaceholderText(
      t.settings.apiKeyPlaceholderConfigured,
    ) as HTMLInputElement;
    expect(apiKeyInput.value).toBe('');
  });

  it('renders disabled inputs and a notice, with no Save button, when not writable', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: false }));
    renderSettings(client);

    expect(screen.getByDisplayValue('http://lidarr:8686')).toBeDisabled();
    expect(screen.getByText(t.settings.notWritableNotice)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.settings.save })).not.toBeInTheDocument();
  });
});

describe('saving settings', () => {
  it('posts exact JSON, omitting the API key when untouched and including it when typed', async () => {
    let capturedBody: Record<string, unknown> | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          capturedBody = JSON.parse(init.body as string) as Record<string, unknown>;
          return Promise.resolve(
            new Response(JSON.stringify({ ok: true, restarting: true }), { status: 200 }),
          );
        }
        if (url === '/api/config') {
          return new Promise(() => {}); // the restart poll never resolves in this test
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await waitFor(() => expect(capturedBody).toBeDefined());
    expect(capturedBody).toEqual({
      lidarrUrl: 'http://lidarr:8686',
      wantedSyncInterval: '5m',
      stallTimeout: '5m',
      maxActive: 3,
      minBitrate: 192,
    });

    const apiKeyInput = screen.getByPlaceholderText(t.settings.apiKeyPlaceholderConfigured);
    fireEvent.change(apiKeyInput, { target: { value: 'new-secret-key' } });
    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await waitFor(() => expect(capturedBody?.lidarrApiKey).toBe('new-secret-key'));
  });

  it('renders a field error from a 422 response beside its field', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                error: 'validation failed',
                fieldErrors: { lidarrUrl: 'must be an absolute http(s) URL' },
              }),
              { status: 422 },
            ),
          );
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await screen.findByText('must be an absolute http(s) URL');
  });

  it('renders the mount hint from a 409 response', async () => {
    const mountHint =
      'config file is not writable; mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount';
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(JSON.stringify({ error: mountHint }), { status: 409 }),
          );
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await screen.findByText(mountHint);
  });

  it('shows the restarting banner after a successful save and clears it once the poll succeeds', async () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(JSON.stringify({ ok: true, restarting: true }), { status: 200 }),
          );
        }
        if (url === '/api/config') {
          // The restart poll: the process has come back up.
          return Promise.resolve(new Response(JSON.stringify(makeConfig()), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText(t.settings.savedRestarting)).toBeInTheDocument();

    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(screen.queryByText(t.settings.savedRestarting)).not.toBeInTheDocument();
  });
});
