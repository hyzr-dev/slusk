import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Job, JobStatus, ModuleStatus, SharesReport, StatusReport } from '../api/types';
import { t } from '../strings';
import Layout from './Layout';
// Imported for its scoped class names only, to disambiguate the dependency
// row's "Soulseek" name from the "Soulseek" nav group heading — the same CSS
// module hashing runs once per file, so this file gets the identical class
// map the component itself renders with.
import layoutStyles from './Layout.module.css';

function makeJob(id: number, status: JobStatus): Job {
  return {
    id,
    title: `Job ${id}`,
    artist: 'Artist',
    status,
    peer: '',
    bytesDone: 0,
    bytesTotal: 0,
    updatedAt: '',
    state: 'DOWNLOADING',
    candidatesTried: 1,
    maxCandidates: 3,
    failReason: '',
    nextAttemptAt: '',
    retries: 0,
    notBefore: '',
    source: 'lidarr',
    year: null,
    tracks: null,
    format: null,
  };
}

function makeModule(overrides: Partial<ModuleStatus> = {}): ModuleStatus {
  return {
    lastAttempt: '',
    lastCompleted: '',
    lastSuccess: '',
    lastErrorAt: '',
    lastError: '',
    consecutiveFailures: 0,
    staleDeadline: '',
    live: true,
    ready: true,
    ...overrides,
  };
}

function makeShares(folderCount: number): SharesReport {
  return {
    enabled: true,
    scanning: false,
    indexedAt: '',
    scanDurationMs: 0,
    directories: 0,
    files: 0,
    totalBytes: 0,
    folders: Array.from({ length: folderCount }, (_, i) => ({
      name: `folder-${i}`,
      path: `/music/${i}`,
      directories: 0,
      files: 0,
      totalBytes: 0,
    })),
  };
}

function renderLayout(opts: { jobs?: Job[]; status?: StatusReport; shares?: SharesReport } = {}) {
  const client = new QueryClient();
  if (opts.jobs) client.setQueryData(queryKeys.jobs, opts.jobs);
  if (opts.status) client.setQueryData(queryKeys.status, opts.status);
  if (opts.shares) client.setQueryData(queryKeys.shares, opts.shares);
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/']}>
        <Routes>
          <Route element={<Layout />}>
            <Route index element={<div>content</div>} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Layout nav groups', () => {
  it('renders each group heading with its items', () => {
    renderLayout();
    // Scoped to the <nav>: the "Soulseek" group heading and the dependency
    // row further down the sidebar share the same exact text.
    const nav = within(document.querySelector(`.${layoutStyles.nav}`) as HTMLElement);
    expect(nav.getByText(t.nav.groupMonitor)).toBeInTheDocument();
    expect(nav.getByText(t.nav.groupSoulseek)).toBeInTheDocument();
    expect(nav.getByText(t.nav.groupSystem)).toBeInTheDocument();

    expect(screen.getByRole('link', { name: /Overview/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /^Jobs/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Events' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Health' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Shares' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Peers' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Settings' })).toBeInTheDocument();
  });

  it('does not render a link for the unbuilt Search or Setup pages', () => {
    renderLayout();
    expect(screen.queryByRole('link', { name: /Search/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Setup/ })).not.toBeInTheDocument();
  });

  it('exposes the nav landmark and each group as an accessible list, not flat links', () => {
    renderLayout();
    const nav = screen.getByRole('navigation', { name: t.nav.navAriaLabel });
    // Three <ul> groups (Monitor/Soulseek/System), each labelled by its own
    // group heading rather than one flat list of seven links.
    const lists = within(nav).getAllByRole('list');
    expect(lists).toHaveLength(3);
    expect(within(lists[0]).getAllByRole('listitem')).toHaveLength(4);
    expect(within(lists[1]).getAllByRole('listitem')).toHaveLength(2);
    expect(within(lists[2]).getAllByRole('listitem')).toHaveLength(1);
  });
});

describe('Layout jobs badge', () => {
  it('shows the sum of active, queued and stalled jobs on the Jobs nav item', () => {
    renderLayout({
      jobs: [makeJob(1, 'active'), makeJob(2, 'queued'), makeJob(3, 'stalled'), makeJob(4, 'done')],
    });
    const jobsLink = screen.getByRole('link', { name: /Jobs/ });
    expect(within(jobsLink).getByText('3')).toBeInTheDocument();
  });

  it('gives the badge an accessible label that reads sensibly out of context', () => {
    renderLayout({
      jobs: [makeJob(1, 'active'), makeJob(2, 'queued'), makeJob(3, 'stalled'), makeJob(4, 'done')],
    });
    // The badge's own text is just "3" — without an aria-label a screen
    // reader would announce the bare number with no indication of what it
    // counts.
    expect(screen.getByRole('link', { name: `Jobs ${t.nav.jobsBadgeLabel(3)}` })).toBeInTheDocument();
  });

  it('hides the badge entirely when there is nothing active, queued or stalled', () => {
    renderLayout({ jobs: [makeJob(1, 'done'), makeJob(2, 'failed')] });
    const jobsLink = screen.getByRole('link', { name: 'Jobs' });
    expect(within(jobsLink).queryByText('0')).not.toBeInTheDocument();
  });
});

describe('Layout dependency dots', () => {
  function dotFor(name: string): HTMLElement {
    const deps = document.querySelector(`.${layoutStyles.deps}`) as HTMLElement;
    return within(deps).getByText(name).previousElementSibling as HTMLElement;
  }

  const attempted = '2026-07-25T00:00:00Z';

  it('marks Lidarr healthy when the wanted_sync module has attempted and is ready', () => {
    renderLayout({
      status: {
        queued: 0,
        active: 0,
        stalled: 0,
        orphaned: 0,
        modules: {},
        moduleDetails: { wanted_sync: makeModule({ ready: true, lastAttempt: attempted }) },
      },
    });
    expect(dotFor(t.nav.depLidarr).className).toMatch(/depOk/);
  });

  it('marks Lidarr unhealthy when the wanted_sync module has attempted but is not ready', () => {
    renderLayout({
      status: {
        queued: 0,
        active: 0,
        stalled: 0,
        orphaned: 0,
        modules: {},
        moduleDetails: { wanted_sync: makeModule({ ready: false, lastAttempt: attempted }) },
      },
    });
    expect(dotFor(t.nav.depLidarr).className).toMatch(/depWarn/);
  });

  it('marks Lidarr as unknown (not unhealthy) when wanted_sync has never attempted a tick', () => {
    // A fresh boot: the module is present in moduleDetails but lastAttempt
    // is still "" — Runner.Health() only computes Ready once Live has been
    // evaluated, which requires a first attempt. This must not read as
    // "down" just because it also isn't "ok" yet.
    renderLayout({
      status: {
        queued: 0,
        active: 0,
        stalled: 0,
        orphaned: 0,
        modules: {},
        moduleDetails: { wanted_sync: makeModule({ ready: false, lastAttempt: '' }) },
      },
    });
    expect(dotFor(t.nav.depLidarr).className).toMatch(/depUnknown/);
    const lidarrRow = dotFor(t.nav.depLidarr).parentElement as HTMLElement;
    expect(within(lidarrRow).getByText(t.nav.depUnknown)).toBeInTheDocument();
  });

  it('marks Lidarr as unknown while the status query has not loaded yet', () => {
    renderLayout();
    expect(dotFor(t.nav.depLidarr).className).toMatch(/depUnknown/);
  });

  it('marks Lidarr as unknown when wanted_sync is absent from moduleDetails entirely', () => {
    renderLayout({
      status: { queued: 0, active: 0, stalled: 0, orphaned: 0, modules: {}, moduleDetails: {} },
    });
    expect(dotFor(t.nav.depLidarr).className).toMatch(/depUnknown/);
  });

  it('marks Soulseek unhealthy when either discovery or downloading is not ready', () => {
    renderLayout({
      status: {
        queued: 0,
        active: 0,
        stalled: 0,
        orphaned: 0,
        modules: {},
        moduleDetails: {
          discovery: makeModule({ ready: true, lastAttempt: attempted }),
          downloading: makeModule({ ready: false, lastAttempt: attempted }),
        },
      },
    });
    expect(dotFor(t.nav.depSoulseek).className).toMatch(/depWarn/);
  });

  it('marks Soulseek healthy when both discovery and downloading are ready', () => {
    renderLayout({
      status: {
        queued: 0,
        active: 0,
        stalled: 0,
        orphaned: 0,
        modules: {},
        moduleDetails: {
          discovery: makeModule({ ready: true, lastAttempt: attempted }),
          downloading: makeModule({ ready: true, lastAttempt: attempted }),
        },
      },
    });
    expect(dotFor(t.nav.depSoulseek).className).toMatch(/depOk/);
  });

  it('marks Shares unhealthy at zero folders and shows the folder count as meta', () => {
    renderLayout({ shares: makeShares(0) });
    expect(dotFor(t.nav.depShares).className).toMatch(/depWarn/);
    expect(screen.getByText(t.nav.depFolders(0), { exact: false })).toBeInTheDocument();
  });

  it('marks Shares healthy with at least one folder', () => {
    renderLayout({ shares: makeShares(3) });
    expect(dotFor(t.nav.depShares).className).toMatch(/depOk/);
    expect(screen.getByText(t.nav.depFolders(3), { exact: false })).toBeInTheDocument();
  });

  it('marks Shares as a neutral, non-alarming state when native sharing is disabled', () => {
    const sharesDisabled = { ...makeShares(0), enabled: false };
    renderLayout({ shares: sharesDisabled });
    expect(dotFor(t.nav.depShares).className).toMatch(/depUnknown/);
    expect(screen.getByText(t.nav.depDisabled)).toBeInTheDocument();
  });

  it('marks Shares as unknown while the shares query has not loaded yet', () => {
    renderLayout();
    expect(dotFor(t.nav.depShares).className).toMatch(/depUnknown/);
  });
});
