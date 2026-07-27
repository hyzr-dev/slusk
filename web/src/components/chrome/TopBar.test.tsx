import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../../api/queries';
import type { StatusReport } from '../../api/types';
import { formatSpeed } from '../../format';
import { t } from '../../strings';
import TopBar, { stalenessLabel } from './TopBar';

const STALE_AFTER = 10_000;

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

describe('build version', () => {
  function renderTopBar(status: Partial<StatusReport> | undefined) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    if (status) client.setQueryData(queryKeys.status, status);
    client.setQueryData(queryKeys.jobs, []);
    client.setQueryData(queryKeys.charts, { passes: [], throughput: [], cumulative: [] });
    return render(
      <QueryClientProvider client={client}>
        <TopBar />
      </QueryClientProvider>,
    );
  }

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
});

// The header's download figure prefers the SSE stream's `down` but must
// survive the stream not being there — see the comment on `down` in
// TopBar.tsx. These guard the two non-obvious cases: a dead stream writes
// null (not undefined) into the live cache, and a healthy but idle stream
// legitimately reports 0.
describe('download speed source', () => {
  function renderWithLive(live: unknown, jobs: unknown[]) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.status, {});
    client.setQueryData(queryKeys.jobs, jobs);
    client.setQueryData(queryKeys.charts, { passes: [], throughput: [], cumulative: [] });
    if (live !== undefined) client.setQueryData(queryKeys.live, live);
    return render(
      <QueryClientProvider client={client}>
        <TopBar />
      </QueryClientProvider>,
    );
  }

  const activeJob = { id: 1, status: 'active', speed: 2048 };

  it('prefers the stream figure over the jobs-derived sum', () => {
    renderWithLive({ jobs: [], down: 4096 }, [activeJob]);
    expect(screen.getByText(formatSpeed(4096))).toBeInTheDocument();
  });

  it('falls back to the jobs sum when the stream has died', () => {
    // clearLive writes null rather than undefined; reading `.down` off that
    // would throw, so this asserts the header still renders at all.
    renderWithLive(null, [activeJob]);
    expect(screen.getByText(formatSpeed(2048))).toBeInTheDocument();
  });

  it('trusts a healthy stream reporting zero rather than falling back', () => {
    // A ?? that mistook 0 for "no data" would show the stale jobs sum here.
    renderWithLive({ jobs: [], down: 0 }, [activeJob]);
    expect(screen.getByText(t.chrome.idle)).toBeInTheDocument();
  });
});
