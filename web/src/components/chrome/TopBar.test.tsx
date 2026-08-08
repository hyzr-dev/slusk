import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { authQueryKeys } from '../../api/auth';
import { queryKeys } from '../../api/queries';
import type { SessionResponse, StatusReport } from '../../api/types';
import { formatSpeed } from '../../format';
import { t } from '../../strings';
import TopBar, { SOURCE_REPO_URL, sourceUrl, stalenessLabel } from './TopBar';

const STALE_AFTER = 10_000;

afterEach(() => vi.unstubAllGlobals());

const AUTHENTICATED_SESSION: SessionResponse = {
  authenticated: true,
  username: 'testuser',
  setupRequired: false,
};

// Pure-function test, not wall-clock dependent: every argument is a plain
// number supplied by the test, never Date.now().
describe('stalenessLabel', () => {
  it('reports nothing before the first successful fetch (dataUpdatedAt is 0)', () => {
    // That state is "no news yet", not evidence that polling has stopped.
    expect(stalenessLabel(0, 1_000_000, STALE_AFTER)).toBeNull();
  });

  it('stays silent while the poll is keeping up', () => {
    // The whole point of the redesign: during normal operation the cell shows
    // no digits at all, so a number appearing there always means trouble.
    expect(stalenessLabel(1_000_000, 1_000_400, STALE_AFTER)).toBeNull();
    expect(stalenessLabel(1_000_000, 1_004_200, STALE_AFTER)).toBeNull();
    expect(stalenessLabel(1_000_000, 1_009_999, STALE_AFTER)).toBeNull();
  });

  it('speaks up once two polls in a row have been missed', () => {
    expect(stalenessLabel(1_000_000, 1_010_000, STALE_AFTER)).toBe(t.chrome.stale('10s'));
  });

  it('coarsens the age past a minute rather than counting seconds forever', () => {
    expect(stalenessLabel(1_000_000, 1_090_000, STALE_AFTER)).toBe(t.chrome.stale('1m'));
    expect(stalenessLabel(1_000, 3_601_000, STALE_AFTER)).toBe(t.chrome.stale('1h'));
  });
});

// AGPL § 13 asks for the source of *the version that is running*, so this maps
// the reported version onto a URL rather than always pointing at the default
// branch. Pure, and tested without rendering, because the fallback branches are
// the interesting part and none of them need a DOM.
describe('sourceUrl', () => {
  it('points a released build at the tree for exactly that tag', () => {
    expect(sourceUrl('v2.5.0')).toBe(`${SOURCE_REPO_URL}/tree/v2.5.0`);
    expect(sourceUrl('v10.0.11')).toBe(`${SOURCE_REPO_URL}/tree/v10.0.11`);
  });

  it('falls back to the repository root for a locally built binary', () => {
    // `dev` is the ldflag default, so it is what every contributor sees.
    expect(sourceUrl('dev')).toBe(SOURCE_REPO_URL);
  });

  it('falls back to the repository root when the server reports no version', () => {
    // A server predating #229 omits the field entirely.
    expect(sourceUrl(undefined)).toBe(SOURCE_REPO_URL);
    expect(sourceUrl('')).toBe(SOURCE_REPO_URL);
  });

  it('refuses to build a URL out of a version string it does not recognise', () => {
    // `version` arrives over HTTP and lands in a URL path, so the shape is
    // checked against what release.yml can actually produce rather than
    // sanitised after the fact.
    expect(sourceUrl('../../../etc/passwd')).toBe(SOURCE_REPO_URL);
    expect(sourceUrl('https://example.com')).toBe(SOURCE_REPO_URL);
    expect(sourceUrl('v1.2')).toBe(SOURCE_REPO_URL);
    expect(sourceUrl('v1.2.3/../..')).toBe(SOURCE_REPO_URL);
    expect(sourceUrl('v1.2.3-rc1')).toBe(SOURCE_REPO_URL);
    // Not hypothetical: a locally built image in testenv/ reported exactly this
    // shape while #391 was being verified in the browser. A build that is not a
    // release has no tree to point at, so it gets the root like `dev` does.
    expect(sourceUrl('lab-2900a32-3ea93caf')).toBe(SOURCE_REPO_URL);
  });
});

describe('TopBar', () => {
  function renderTopBar(status: Partial<StatusReport> | undefined) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    if (status) client.setQueryData(queryKeys.status, status);
    client.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: [],
      uploadThroughput: [],
    });
    client.setQueryData(authQueryKeys.session, AUTHENTICATED_SESSION);
    return render(
      <QueryClientProvider client={client}>
        <TopBar />
      </QueryClientProvider>,
    );
  }

  it('makes the status region keyboard reachable', () => {
    renderTopBar({});
    expect(screen.getByRole('region', { name: t.chrome.statusRegion })).toHaveAttribute(
      'tabindex',
      '0',
    );
  });

  it('shows the version the server reports, beside the product name', () => {
    renderTopBar({ version: 'v1.33.4' });
    expect(screen.getByText('v1.33.4')).toBeInTheDocument();
  });

  it('shows dev for a binary built without the ldflag', () => {
    renderTopBar({ version: 'dev' });
    expect(screen.getByText('dev')).toBeInTheDocument();
  });

  it('renders no version slot at all when the server omits the field', () => {
    // A server predating #229 sends no `version`. An empty span beside the
    // name would read as a bug rather than as absent information.
    const { container } = renderTopBar({});
    expect(container.querySelector('[class*="brandVersion"]')).toBeNull();
  });

  it('offers the source for the running version, as AGPL § 13 requires', () => {
    renderTopBar({ version: 'v1.33.4' });
    const link = screen.getByRole('link', { name: t.chrome.sourceLabelAccessible });
    expect(link).toHaveAttribute('href', `${SOURCE_REPO_URL}/tree/v1.33.4`);
  });

  it('still offers the source when the server reports no version at all', () => {
    // The version slot is conditional; the § 13 offer is not. An instance
    // running a server that omits `version` must still be able to get at the
    // source, so this link renders unconditionally and points at the root.
    renderTopBar({});
    expect(screen.getByRole('link', { name: t.chrome.sourceLabelAccessible })).toHaveAttribute(
      'href',
      SOURCE_REPO_URL,
    );
  });

  it('opens the source in a new tab without leaking the dashboard URL', () => {
    // The app's first external link, so this sets the house convention:
    // a dashboard someone is troubleshooting on should not be navigated away
    // from, and `noreferrer` keeps a private instance's address off the wire.
    renderTopBar({ version: 'v1.33.4' });
    const link = screen.getByRole('link', { name: t.chrome.sourceLabelAccessible });
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('shows the logged-in username next to the logout control', () => {
    renderTopBar({});
    expect(screen.getByText('testuser')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.auth.logout })).toBeInTheDocument();
  });

  it('omits the username entirely for a bearer-token session, not as an empty gap', () => {
    // The `make dev` case: the Vite proxy injects a bearer token, so the
    // session reports authenticated:true with no username at all.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.status, {});
    client.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: [],
      uploadThroughput: [],
    });
    client.setQueryData(authQueryKeys.session, {
      authenticated: true,
      username: null,
      setupRequired: false,
    } satisfies SessionResponse);
    const { container } = render(
      <QueryClientProvider client={client}>
        <TopBar />
      </QueryClientProvider>,
    );
    expect(container.querySelector('[class*="username"]')).toBeNull();
    expect(screen.getByRole('button', { name: t.auth.logout })).toBeInTheDocument();
  });
});

// Each header direction independently prefers its SSE figure but survives the
// stream not being there. A healthy stream's explicit zero means idle and must
// not fall back to a stale non-zero REST sample.
describe('throughput speed sources', () => {
  function renderWithLive(live: unknown, down = 2048, up = 3072) {
    const fetchMock = vi.fn((_url: string) => new Promise(() => {}));
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.status, {});
    client.setQueryData(authQueryKeys.session, AUTHENTICATED_SESSION);
    client.setQueryData(queryKeys.charts, {
      passes: [],
      completedByHour: [],
      throughput: [{ at: '2026-01-01T00:00:00Z', bytesPerSecond: down, activeTransfers: 1 }],
      uploadThroughput: [{ at: '2026-01-01T00:00:00Z', bytesPerSecond: up, activeTransfers: 1 }],
    });
    if (live !== undefined) client.setQueryData(queryKeys.live, live);
    const rendered = render(
      <QueryClientProvider client={client}>
        <TopBar />
      </QueryClientProvider>,
    );
    return { ...rendered, fetchMock };
  }

  it('renders DOWN then UP and prefers each stream figure', () => {
    const { container } = renderWithLive({ jobs: [], down: 4096, up: 8192 });
    const speedCell = Array.from(container.querySelectorAll('[class*="cell"]'))
      .find((cell) => cell.textContent?.startsWith(t.chrome.down));
    expect(speedCell).toHaveTextContent('DOWN 4 KB/s · UP 8 KB/s');
  });

  it('falls back to each latest REST sample when the stream has died', () => {
    renderWithLive(null);
    expect(screen.getByText(formatSpeed(2048))).toBeInTheDocument();
    expect(screen.getByText(formatSpeed(3072))).toBeInTheDocument();
  });

  it('falls back independently when only one stream direction is absent', () => {
    renderWithLive({ jobs: [], down: 4096 });
    expect(screen.getByText(formatSpeed(4096))).toBeInTheDocument();
    expect(screen.getByText(formatSpeed(3072))).toBeInTheDocument();
  });

  it('does not fetch the all-jobs endpoint from persistent chrome', () => {
    const { fetchMock } = renderWithLive(null);
    expect(fetchMock.mock.calls.some(([url]) => url === '/api/jobs/all')).toBe(false);
  });

  it('trusts explicit zero for each stream direction rather than falling back', () => {
    renderWithLive({ jobs: [], down: 0, up: 0 });
    expect(screen.getAllByText(t.chrome.idle)).toHaveLength(2);
    expect(screen.queryByText(formatSpeed(2048))).not.toBeInTheDocument();
    expect(screen.queryByText(formatSpeed(3072))).not.toBeInTheDocument();
  });
});
