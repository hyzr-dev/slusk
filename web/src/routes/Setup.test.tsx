import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { AppConfig, SharesReport } from '../api/types';
import { t } from '../strings';
import Setup from './Setup';

afterEach(() => vi.unstubAllGlobals());

function makeConfig(overrides: Partial<AppConfig> = {}): AppConfig {
  return {
    lidarr: { url: 'http://lidarr:8686', apiKeyConfigured: true },
    slskd: { url: 'http://slskd:5030', apiKeyConfigured: true },
    // Setup never reads pipeline fields, so a real shape isn't needed here.
    pipeline: {} as AppConfig['pipeline'],
    soulseek: {
      enabled: true,
      serverAddress: 'server.slsknet.org:2242',
      username: 'testuser',
      passwordConfigured: true,
      listenAddr: ':2234',
      uploadSlots: 2,
      allowPrivatePeerAddresses: false,
      gluetun: { controlUrl: '', apiKeyConfigured: false },
      sharedFolders: [{ name: 'Library', path: '/music' }],
    },
    store: { dsnConfigured: false },
    observ: { listenAddr: ':9090', authTokenConfigured: false, logLevel: '' },
    paths: { slskdCompleteDir: '/downloads' },
    writable: true,
    ...overrides,
  };
}

function makeShares(overrides: Partial<SharesReport> = {}): SharesReport {
  return {
    enabled: true,
    scanning: false,
    indexedAt: '',
    scanDurationMs: 0,
    directories: 0,
    files: 0,
    totalBytes: 0,
    folders: [],
    ...overrides,
  };
}

// Config and shares are seeded straight into the cache: both queries have
// staleTime/refetchInterval semantics that make a real fetch unnecessary for
// these assertions, and stubbing fetch throughout would only obscure that the
// values under test come from cached data, not a network response.
function renderSetup(config: AppConfig, shares: SharesReport) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(queryKeys.config, config);
  client.setQueryData(queryKeys.shares, shares);
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Setup />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Setup', () => {
  it('never renders a secret, only whether it is configured', async () => {
    renderSetup(makeConfig(), makeShares());
    expect((await screen.findAllByText(t.setup.secretSet)).length).toBeGreaterThan(0);
    expect(screen.queryByText(/ab12/)).not.toBeInTheDocument();
    // The password itself must never appear even though the fixture username
    // does — password is intentionally omitted from AppConfig entirely, but
    // this guards the case where a future fixture accidentally carries one.
    expect(screen.queryByText('hunter2')).not.toBeInTheDocument();
  });

  it('derives the shares step from the index rather than a test call', async () => {
    // Soulseek's own (untested) connection step also reads UNTESTED here, so
    // this asserts on the shares-specific explanation text rather than the
    // shared state label, and confirms OK never appears for an empty index.
    renderSetup(makeConfig(), makeShares({ files: 0 }));
    expect(await screen.findByText(t.setup.sharesNoTest)).toBeInTheDocument();
    expect(screen.queryByText(t.setup.stateOk)).not.toBeInTheDocument();
  });

  it('shows the shares step as OK once the index has found files', async () => {
    renderSetup(makeConfig(), makeShares({ files: 42 }));
    expect(await screen.findByText(t.setup.stateOk)).toBeInTheDocument();
  });

  it('reports soulseek as not enabled when the backend is off, and hides its test button', async () => {
    const config = makeConfig();
    renderSetup({ ...config, soulseek: { ...config.soulseek, enabled: false } }, makeShares());
    expect(await screen.findByText(t.setup.stateDisabled)).toBeInTheDocument();
    // Only Lidarr's TEST button remains once Soulseek's is hidden.
    expect(screen.getAllByRole('button', { name: t.setup.test })).toHaveLength(1);
  });

  it('shows FAILED and the returned error inside the error card when a probe reports ok: false', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url === '/api/config/test/lidarr') {
          return Promise.resolve(
            new Response(JSON.stringify({ ok: false, error: 'lidarr unreachable: connection refused' }), {
              status: 200,
            }),
          );
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    renderSetup(makeConfig(), makeShares());

    // Scope to Lidarr's own step: Soulseek renders a TEST button too, and both
    // start UNTESTED, so asserting against the whole document would leave the
    // click ambiguous about which dependency it exercised.
    const lidarrHeader = screen.getByText(new RegExp(t.setup.stepLidarr)).closest('div');
    if (!lidarrHeader) throw new Error('lidarr step header not found');
    fireEvent.click(within(lidarrHeader).getByRole('button', { name: t.setup.test }));

    expect(await screen.findByText(t.setup.stateFailed)).toBeInTheDocument();
    expect(screen.getByText('lidarr unreachable: connection refused')).toBeInTheDocument();
  });
});
