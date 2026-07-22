import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { AppConfig } from '../api/types';
import { t } from '../strings';
import Settings from './Settings';

afterEach(() => vi.unstubAllGlobals());

function makeConfig(overrides: Partial<AppConfig> = {}): AppConfig {
  return {
    lidarrUrl: 'http://lidarr:8686',
    lidarrApiKeyConfigured: true,
    wantedSyncInterval: '5m',
    maxActive: 3,
    soulseekEnabled: false,
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
