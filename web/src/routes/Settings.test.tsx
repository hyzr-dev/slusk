import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { AppConfig, SoulseekConfigDTO } from '../api/types';
import { t } from '../strings';
import Settings from './Settings';

// jsdom has no layout engine and doesn't implement scrollIntoView at all.
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const soulseekBase: SoulseekConfigDTO = {
  enabled: true,
  serverAddress: 'server.slsknet.org:2242',
  username: 'slskuser',
  passwordConfigured: true,
  listenAddr: '0.0.0.0:2234',
  uploadSlots: 2,
  allowPrivatePeerAddresses: false,
  gluetun: { controlUrl: 'http://127.0.0.1:8000', apiKeyConfigured: false },
  sharedFolders: [{ name: 'Music', path: '/shares/music' }],
};

function makeConfig(overrides: Partial<AppConfig> = {}): AppConfig {
  return {
    lidarr: { url: 'http://lidarr:8686', apiKeyConfigured: true },
    slskd: { url: 'http://slskd:5030', apiKeyConfigured: true },
    pipeline: {
      backend: 'slskd',
      maxCandidatesPerAlbum: 5,
      maxActive: 30,
      maxRetries: 10,
      maxInflightPerPeer: 3,
      maxTransferRetries: 3,
      minBitrate: 192,
      transferDeadline: '1h0m0s',
      stallTimeout: '5m0s',
      searchTimeout: '45s',
      backoffBase: '15m0s',
      backoffCap: '24h0m0s',
      candidateTtl: '24h0m0s',
      failedReviveAfter: '720h0m0s',
      stuckAfter: '1h0m0s',
      tickTimeout: '5m0s',
      importConfirmTimeout: '3m0s',
      wantedSyncInterval: '15m0s',
      discoveryInterval: '30s',
      selectingInterval: '10s',
      downloadingInterval: '15s',
      importingInterval: '30s',
      manualImportTimeout: '10m0s',
      importRetryCooldown: '5m0s',
      weights: { format: 1.0, bitrate: 1.0, reliability: 1.0, fileCount: 1.0, knownUser: 1.0 },
    },
    soulseek: soulseekBase,
    store: { dsnConfigured: true },
    observ: { listenAddr: ':9090', authTokenConfigured: true, logLevel: 'info' },
    paths: { slskdCompleteDir: '/music/slskd-downloads' },
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

// cardFor scopes a query to one section card, since several cards share
// generic field labels (both Lidarr and slskd have "URL" and "API key").
function cardFor(title: string): HTMLElement {
  const heading = screen.getByRole('heading', { name: title });
  return heading.closest('section') as HTMLElement;
}

// Every card is a collapsed accordion by default; open it before touching
// any of its fields. While collapsed, the section's own toggle button is
// the only aria-expanded button in the card (the Advanced disclosure, if
// any, doesn't exist in the DOM until the card itself is open).
function openSection(title: string) {
  fireEvent.click(within(cardFor(title)).getByRole('button', { expanded: false }));
}

function openAdvanced(sectionTitle: string) {
  fireEvent.click(within(cardFor(sectionTitle)).getByRole('button', { name: t.settings.advanced }));
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
    client.setQueryData(queryKeys.config, makeConfig({ soulseek: { ...soulseekBase, enabled: false } }));
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
    client.setQueryData(queryKeys.config, makeConfig({ soulseek: { ...soulseekBase, enabled: false } }));
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
    client.setQueryData(queryKeys.config, makeConfig({ soulseek: { ...soulseekBase, enabled: false } }));
    renderSettings(client);

    fireEvent.click(screen.getByRole('button', { name: t.settings.testConnection }));
    await screen.findByText(t.settings.testStatus.failure);
    expect(screen.getByText(t.settings.testUnreachable)).toBeInTheDocument();
  });

  it('offers the Soulseek test only when soulseek is enabled', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));

    const disabled = newClient();
    disabled.setQueryData(queryKeys.config, makeConfig({ soulseek: { ...soulseekBase, enabled: false } }));
    const { unmount } = renderSettings(disabled);
    expect(screen.getAllByRole('button', { name: t.settings.testConnection })).toHaveLength(1);
    unmount();

    const enabled = newClient();
    enabled.setQueryData(queryKeys.config, makeConfig({ soulseek: { ...soulseekBase, enabled: true } }));
    renderSettings(enabled);
    expect(screen.getAllByRole('button', { name: t.settings.testConnection })).toHaveLength(2);
  });
});

describe('rendering', () => {
  it('renders every section card from a full config fixture', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    expect(screen.getByRole('heading', { name: t.settings.lidarr })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.slskd })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.pipeline })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.soulseek })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.observability })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.dangerZone })).toBeInTheDocument();

    // Weights render inside Pipeline's Advanced disclosure.
    openSection(t.settings.pipeline);
    openAdvanced(t.settings.pipeline);
    expect(within(cardFor(t.settings.pipeline)).getByRole('heading', { name: t.settings.weights })).toBeInTheDocument();

    openSection(t.settings.soulseek);
    openAdvanced(t.settings.soulseek);
    const soulseekCard = cardFor(t.settings.soulseek);
    expect(within(soulseekCard).getByText(t.settings.gluetunTitle)).toBeInTheDocument();
    expect(within(soulseekCard).getByDisplayValue('Music')).toBeInTheDocument();
    expect(within(soulseekCard).getByDisplayValue('/shares/music')).toBeInTheDocument();
  });

  it('renders disabled inputs and no Save button anywhere when not writable', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: false }));
    renderSettings(client);

    expect(screen.getByText(t.settings.notWritableNotice)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.settings.save })).not.toBeInTheDocument();

    openSection(t.settings.lidarr);
    const lidarrCard = cardFor(t.settings.lidarr);
    expect(within(lidarrCard).getByDisplayValue('http://lidarr:8686')).toBeDisabled();

    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    expect(within(pipelineCard).getByDisplayValue('30')).toBeDisabled();

    openSection(t.settings.dangerZone);
    const dangerCard = cardFor(t.settings.dangerZone);
    expect(within(dangerCard).getByDisplayValue('/music/slskd-downloads')).toBeDisabled();
  });

  it('renders every top-level section collapsed by default, with no inputs until opened', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    // Headers are always visible…
    expect(screen.getByRole('heading', { name: t.settings.lidarr })).toBeInTheDocument();
    // …but no field input exists anywhere yet, since every section body is
    // conditionally rendered rather than merely hidden.
    expect(screen.queryByDisplayValue('http://lidarr:8686')).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue('30')).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue('/music/slskd-downloads')).not.toBeInTheDocument();

    openSection(t.settings.lidarr);
    expect(within(cardFor(t.settings.lidarr)).getByDisplayValue('http://lidarr:8686')).toBeInTheDocument();
  });

  it('keeps a section\'s advanced fields hidden until its Advanced disclosure is opened', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    // maxActive (basic) is visible; maxRetries and the weights (advanced) are not.
    expect(within(pipelineCard).getByDisplayValue('30')).toBeInTheDocument();
    expect(within(pipelineCard).queryByDisplayValue('10')).not.toBeInTheDocument();
    expect(within(pipelineCard).queryByText(t.settings.weights)).not.toBeInTheDocument();

    openAdvanced(t.settings.pipeline);
    expect(within(pipelineCard).getByDisplayValue('10')).toBeInTheDocument();
    expect(within(pipelineCard).getByText(t.settings.weights)).toBeInTheDocument();
    // All five weight fixture values are 1.0 (String(1.0) === '1').
    expect(within(pipelineCard).getAllByDisplayValue('1')).toHaveLength(5);
  });

  it('shows the loading line above the connections section while config has never resolved', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    renderSettings(client);

    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.connections })).toBeInTheDocument();
  });

  it('shows the failed line above the connections section when the config fetch never succeeds', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('boom'))));
    const client = newClient();
    renderSettings(client);

    expect(await screen.findByText(t.query.failed)).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: t.settings.connections })).toBeInTheDocument();
  });
});

describe('saving settings', () => {
  it('posts the full nested body, omitting secrets when untouched and including them when typed', async () => {
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
      lidarr: { url: 'http://lidarr:8686' },
      slskd: { url: 'http://slskd:5030' },
      pipeline: {
        backend: 'slskd',
        maxCandidatesPerAlbum: 5,
        maxActive: 30,
        maxRetries: 10,
        maxInflightPerPeer: 3,
        maxTransferRetries: 3,
        minBitrate: 192,
        transferDeadline: '1h0m0s',
        stallTimeout: '5m0s',
        searchTimeout: '45s',
        backoffBase: '15m0s',
        backoffCap: '24h0m0s',
        candidateTtl: '24h0m0s',
        failedReviveAfter: '720h0m0s',
        stuckAfter: '1h0m0s',
        tickTimeout: '5m0s',
        importConfirmTimeout: '3m0s',
        wantedSyncInterval: '15m0s',
        discoveryInterval: '30s',
        selectingInterval: '10s',
        downloadingInterval: '15s',
        importingInterval: '30s',
        manualImportTimeout: '10m0s',
        importRetryCooldown: '5m0s',
        weights: { format: 1, bitrate: 1, reliability: 1, fileCount: 1, knownUser: 1 },
      },
      soulseek: {
        serverAddress: 'server.slsknet.org:2242',
        username: 'slskuser',
        listenAddr: '0.0.0.0:2234',
        uploadSlots: 2,
        allowPrivatePeerAddresses: false,
        gluetun: { controlUrl: 'http://127.0.0.1:8000' },
        sharedFolders: [{ name: 'Music', path: '/shares/music' }],
      },
      store: {},
      observ: { listenAddr: ':9090', logLevel: 'info' },
      paths: { slskdCompleteDir: '/music/slskd-downloads' },
    });

    // Two of the six write-only secrets: lidarr.apiKey and soulseek.password.
    openSection(t.settings.lidarr);
    const lidarrApiKeyInput = within(cardFor(t.settings.lidarr)).getByPlaceholderText(
      t.settings.secretPlaceholderConfigured,
    );
    fireEvent.change(lidarrApiKeyInput, { target: { value: 'new-lidarr-key' } });

    // exact: false because the accessible label text includes the trailing
    // "(password)" TOML-key hint rendered inside the same <label>.
    openSection(t.settings.soulseek);
    const soulseekPasswordInput = within(cardFor(t.settings.soulseek)).getByLabelText(
      t.settings.password,
      { exact: false, selector: 'input' },
    );
    fireEvent.change(soulseekPasswordInput, { target: { value: 'new-soulseek-password' } });

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await waitFor(() => expect((capturedBody as { lidarr: { apiKey?: string } }).lidarr.apiKey).toBe('new-lidarr-key'));
    expect((capturedBody as { soulseek: { password?: string } }).soulseek.password).toBe(
      'new-soulseek-password',
    );
  });

  it('reflects shared-folder add/remove in the exact POST array', async () => {
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
        if (url === '/api/config') return new Promise(() => {});
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    openSection(t.settings.soulseek);
    const soulseekCard = cardFor(t.settings.soulseek);
    fireEvent.click(within(soulseekCard).getByRole('button', { name: t.settings.addFolder }));

    const nameInputs = within(soulseekCard).getAllByLabelText(t.settings.folderName);
    const pathInputs = within(soulseekCard).getAllByLabelText(t.settings.folderPath);
    expect(nameInputs).toHaveLength(2);
    fireEvent.change(nameInputs[1], { target: { value: 'Live' } });
    fireEvent.change(pathInputs[1], { target: { value: '/shares/live' } });

    // Remove the original "Music" row (index 0), keeping only the new one.
    fireEvent.click(within(soulseekCard).getAllByRole('button', { name: t.settings.removeFolder })[0]);

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await waitFor(() => expect(capturedBody).toBeDefined());
    expect((capturedBody as { soulseek: { sharedFolders: unknown } }).soulseek.sharedFolders).toEqual([
      { name: 'Live', path: '/shares/live' },
    ]);
  });

  it('requires two Save clicks only when a danger-zone field is touched', async () => {
    let postCount = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          postCount += 1;
          return Promise.resolve(
            new Response(JSON.stringify({ ok: true, restarting: true }), { status: 200 }),
          );
        }
        if (url === '/api/config') return new Promise(() => {});
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    // Untouched danger zone: a pipeline-only change submits on the first click.
    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    fireEvent.change(within(pipelineCard).getByDisplayValue('30'), { target: { value: '40' } });
    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await waitFor(() => expect(postCount).toBe(1));

    // Touching store.dsn arms the button; the first click after that must not submit.
    openSection(t.settings.dangerZone);
    const dangerCard = cardFor(t.settings.dangerZone);
    // exact: false because the accessible label text includes the trailing
    // "(dsn)" TOML-key hint rendered inside the same <label>.
    const dsnInput = within(dangerCard).getByLabelText(t.settings.dsn, { exact: false, selector: 'input' });
    fireEvent.change(dsnInput, { target: { value: 'postgres://new' } });

    // First click after touching the danger zone only arms the button (still
    // labeled Save at the moment of the click) — it must not submit yet.
    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    expect(postCount).toBe(1);
    await screen.findByRole('button', { name: t.settings.saveConfirm });

    fireEvent.click(screen.getByRole('button', { name: t.settings.saveConfirm }));
    await waitFor(() => expect(postCount).toBe(2));
  });

  it('renders a nested 422 field error next to its field', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                error: 'validation failed',
                fieldErrors: { 'pipeline.maxActive': 'must be >= 1' },
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
    await screen.findByText('must be >= 1');
    expect(within(cardFor(t.settings.pipeline)).getByText('must be >= 1')).toBeInTheDocument();
  });

  it('renders a cross-field 422 (empty fieldErrors) as a general save-error banner', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                error: 'pipeline.backend=soulseek requires a configured soulseek section',
                fieldErrors: {},
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
    await screen.findByText('pipeline.backend=soulseek requires a configured soulseek section');
  });

  it('falls back to the save-error banner when no 422 fieldError key maps to a rendered field', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                error: 'validation failed',
                fieldErrors: { 'future.unknownKey': 'nope' },
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
    // No section can auto-expand for an unlocatable key, so without the
    // banner the failed save would be completely silent.
    await screen.findByText('validation failed');
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

  it('re-seeds the form from the refetched config after a save, clearing the changed badge', async () => {
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
          // Both the restart poll and the post-invalidate refetch: the saved
          // config comes back with the backend's canonical duration forms.
          return Promise.resolve(new Response(JSON.stringify(makeConfig()), { status: 200 }));
        }
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    // "5m" is what a user types; the backend saves it but echoes the
    // canonical "5m0s" — without the post-save re-seed, comparing "5m"
    // against "5m0s" would leave the changed badge stuck forever.
    fireEvent.change(within(pipelineCard).getByDisplayValue('5m0s'), { target: { value: '5m' } });
    expect(within(pipelineCard).getByText(t.settings.changedBadge)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText(t.settings.savedRestarting)).toBeInTheDocument();

    await act(() => vi.advanceTimersByTimeAsync(2000)); // poll succeeds → invalidate → refetch
    await act(() => vi.advanceTimersByTimeAsync(0)); // flush the refetch round trip
    expect(within(pipelineCard).getByDisplayValue('5m0s')).toBeInTheDocument();
    expect(screen.queryByText(t.settings.changedBadge)).not.toBeInTheDocument();
  });

  it('shows a local error for a decimal in an integer field, without posting', async () => {
    const fetchMock = vi.fn(() => Promise.reject(new Error('fetch should not be called')));
    vi.stubGlobal('fetch', fetchMock);
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    fireEvent.change(within(pipelineCard).getByDisplayValue('30'), { target: { value: '2.5' } });

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await screen.findByText(t.settings.mustBeWholeNumber);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows a local "required" error for an emptied numeric field, without posting', async () => {
    const fetchMock = vi.fn(() => Promise.reject(new Error('fetch should not be called')));
    vi.stubGlobal('fetch', fetchMock);
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    // Weights render inside Pipeline's Advanced disclosure.
    openSection(t.settings.pipeline);
    openAdvanced(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    // exact: false because the accessible label text includes the trailing
    // "(format)" TOML-key hint rendered inside the same <label>.
    const formatWeightInput = within(pipelineCard).getByLabelText(t.settings.weightFormat, { exact: false, selector: 'input' });
    fireEvent.change(formatWeightInput, { target: { value: '' } });

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await screen.findByText(t.settings.fieldRequired);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('reaches the POST body for fields inside an opened Advanced disclosure', async () => {
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
        if (url === '/api/config') return new Promise(() => {});
        return Promise.reject(new Error(`unexpected fetch ${url}`));
      }),
    );
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    openSection(t.settings.pipeline);
    openAdvanced(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);

    // maxRetries (advanced field) and format weight (advancedExtra), both
    // only reachable once Pipeline's Advanced disclosure is open.
    fireEvent.change(within(pipelineCard).getByDisplayValue('10'), { target: { value: '7' } });
    const formatWeightInput = within(pipelineCard).getByLabelText(t.settings.weightFormat, { exact: false, selector: 'input' });
    fireEvent.change(formatWeightInput, { target: { value: '2.5' } });

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await waitFor(() => expect(capturedBody).toBeDefined());
    const pipelineBody = (
      capturedBody as { pipeline: { maxRetries: number; weights: { format: number } } }
    ).pipeline;
    expect(pipelineBody.maxRetries).toBe(7);
    expect(pipelineBody.weights.format).toBe(2.5);
  });

  it('auto-expands the section and its Advanced disclosure on a 422 for an advanced field, and scrolls to it', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        if (url === '/api/config' && init?.method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                error: 'validation failed',
                fieldErrors: { 'pipeline.maxRetries': 'must be >= 1' },
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

    // Make a basic-field change, then collapse again, so the save attempt
    // below starts from an all-collapsed state, same as a returning user.
    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    fireEvent.change(within(pipelineCard).getByDisplayValue('30'), { target: { value: '40' } });
    fireEvent.click(within(pipelineCard).getByRole('button', { expanded: true }));
    expect(within(pipelineCard).queryByDisplayValue('40')).not.toBeInTheDocument();

    const scrollSpy = Element.prototype.scrollIntoView as ReturnType<typeof vi.fn>;
    scrollSpy.mockClear();

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await screen.findByText('must be >= 1');

    // Both the section and its Advanced disclosure re-expanded.
    expect(within(pipelineCard).getByDisplayValue('40')).toBeInTheDocument();
    expect(within(pipelineCard).getByText('must be >= 1')).toBeInTheDocument();
    expect(scrollSpy).toHaveBeenCalled();
  });

  it('auto-expands its section again for a local validation error after being collapsed', async () => {
    const fetchMock = vi.fn(() => Promise.reject(new Error('fetch should not be called')));
    vi.stubGlobal('fetch', fetchMock);
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig({ writable: true }));
    renderSettings(client);

    openSection(t.settings.pipeline);
    const pipelineCard = cardFor(t.settings.pipeline);
    fireEvent.change(within(pipelineCard).getByDisplayValue('30'), { target: { value: '' } });
    fireEvent.click(within(pipelineCard).getByRole('button', { expanded: true }));
    expect(within(pipelineCard).getByRole('button', { expanded: false })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.settings.save }));
    await screen.findByText(t.settings.fieldRequired);
    expect(within(pipelineCard).getByRole('button', { expanded: true })).toBeInTheDocument();
    expect(within(pipelineCard).getByText(t.settings.fieldRequired)).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('help popovers', () => {
  it('toggles a field help popover via its button and via Escape, updating aria-expanded', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    openSection(t.settings.lidarr);
    const lidarrCard = cardFor(t.settings.lidarr);
    // Each help button carries its field's label ("Help: URL"), so keyboard
    // and screen-reader users can tell ~40 otherwise identical buttons apart.
    const urlHelpButton = within(lidarrCard).getByRole('button', {
      name: `${t.settings.helpButtonLabel}: ${t.settings.url}`,
    });

    expect(urlHelpButton).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText(t.settings.help.lidarrUrl)).not.toBeInTheDocument();

    fireEvent.click(urlHelpButton);
    expect(urlHelpButton).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText(t.settings.help.lidarrUrl)).toBeInTheDocument();

    // Click again closes it.
    fireEvent.click(urlHelpButton);
    expect(urlHelpButton).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText(t.settings.help.lidarrUrl)).not.toBeInTheDocument();

    // Escape closes it too.
    fireEvent.click(urlHelpButton);
    expect(screen.getByText(t.settings.help.lidarrUrl)).toBeInTheDocument();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(urlHelpButton).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText(t.settings.help.lidarrUrl)).not.toBeInTheDocument();
  });

  it('closes an open popover on a pointer press outside it', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    openSection(t.settings.lidarr);
    fireEvent.click(
      within(cardFor(t.settings.lidarr)).getByRole('button', {
        name: `${t.settings.helpButtonLabel}: ${t.settings.url}`,
      }),
    );
    expect(screen.getByText(t.settings.help.lidarrUrl)).toBeInTheDocument();

    fireEvent.mouseDown(document.body);
    expect(screen.queryByText(t.settings.help.lidarrUrl)).not.toBeInTheDocument();
  });
});

describe('changed badge', () => {
  it('appears only on the edited section, survives a collapse, and clears on revert', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const client = newClient();
    client.setQueryData(queryKeys.config, makeConfig());
    renderSettings(client);

    expect(screen.queryByText(t.settings.changedBadge)).not.toBeInTheDocument();

    openSection(t.settings.lidarr);
    const lidarrCard = cardFor(t.settings.lidarr);
    expect(within(lidarrCard).queryByText(t.settings.changedBadge)).not.toBeInTheDocument();

    fireEvent.change(within(lidarrCard).getByDisplayValue('http://lidarr:8686'), {
      target: { value: 'http://lidarr:9999' },
    });
    expect(within(lidarrCard).getByText(t.settings.changedBadge)).toBeInTheDocument();

    // Only the edited section is marked dirty.
    const slskdCard = cardFor(t.settings.slskd);
    expect(within(slskdCard).queryByText(t.settings.changedBadge)).not.toBeInTheDocument();

    // Collapsing the section keeps the badge visible on its header.
    fireEvent.click(within(lidarrCard).getByRole('button', { expanded: true }));
    expect(within(lidarrCard).getByText(t.settings.changedBadge)).toBeInTheDocument();

    // Reopening and reverting the value clears it.
    fireEvent.click(within(lidarrCard).getByRole('button', { expanded: false }));
    fireEvent.change(within(lidarrCard).getByDisplayValue('http://lidarr:9999'), {
      target: { value: 'http://lidarr:8686' },
    });
    expect(within(lidarrCard).queryByText(t.settings.changedBadge)).not.toBeInTheDocument();
  });
});
