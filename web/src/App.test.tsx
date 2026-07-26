import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import Layout from './components/Layout';
import Overview from './routes/Overview';
import Jobs from './routes/Jobs';
import JobDetail from './routes/JobDetail';
import Events from './routes/Events';
import Peers from './routes/Peers';
import Shares from './routes/Shares';
import Health from './routes/Health';
import Settings from './routes/Settings';
import { t } from './strings';

// Renders the real route tree (mirroring App.tsx) at each path with a
// MemoryRouter, proving every one of the eight routes mounts without
// crashing. BrowserRouter itself is exercised implicitly by the build
// (the SPA is served by the Go backend, which handles history fallback).
function renderAt(path: string) {
  const queryClient = new QueryClient();
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route element={<Layout />}>
            <Route index element={<Overview />} />
            <Route path="jobs" element={<Jobs />} />
            <Route path="jobs/:id" element={<JobDetail />} />
            <Route path="events" element={<Events />} />
            <Route path="peers" element={<Peers />} />
            <Route path="shares" element={<Shares />} />
            <Route path="health" element={<Health />} />
            <Route path="settings" element={<Settings />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('route tree', () => {
  // Every route below is asserted on stable visible text. Layout renders a
  // single visually-hidden <h1> per route (see the dedicated heading tests
  // further down) — PageHeading is gone everywhere, but the heading itself
  // isn't, so these checks focus on each route's own content instead of
  // duplicating that coverage per route.
  it('renders /settings without crashing', () => {
    // No seeded query data, so useConfig() never resolves and the whole
    // ConfigForm stays unrendered — the static Connections section (which
    // doesn't depend on config) is the only thing guaranteed to render.
    renderAt('/settings');
    expect(screen.getByText(t.settings.connections)).toBeInTheDocument();
  });

  it('renders / (Overview) without crashing', () => {
    renderAt('/');
    expect(screen.getByText(t.overview.transfersHeading)).toBeInTheDocument();
  });

  it('renders /jobs without crashing', () => {
    renderAt('/jobs');
    expect(screen.getByText(t.jobs.gridHead.album)).toBeInTheDocument();
  });

  it('renders /events without crashing', () => {
    // No seeded query data, so the filter box and header row are the only
    // things guaranteed to render regardless of what useEvents resolves to.
    renderAt('/events');
    expect(screen.getByText(t.columns.time)).toBeInTheDocument();
  });

  it('renders /peers without crashing', () => {
    // No seeded query data, so the header row is the only thing guaranteed
    // to render regardless of what usePeers resolves to.
    renderAt('/peers');
    expect(screen.getByText(t.peers.gridHead.peer)).toBeInTheDocument();
  });

  it('exposes the events grid as an ARIA table with four column headers', () => {
    // No seeded query data, so the header row's columnheaders are the only
    // thing this test can honestly assert; the table is unnamed (Layout's
    // own <h1> already names the page), so getByRole('table') is unambiguous
    // on its own. Cell coverage with actual rows lives in Peers.test.tsx,
    // which has seeded data to assert against.
    renderAt('/events');
    const table = screen.getByRole('table');
    expect(within(table).getAllByRole('columnheader')).toHaveLength(4);
    expect(within(table).queryAllByRole('cell')).toHaveLength(0);
  });

  it('renders /health without crashing', () => {
    // No seeded query data — the METRICS SectionHeader label is static and
    // renders regardless of what useStatus/useCharts/useUploads/useShares
    // resolve to, unlike the dependency cards and chart panels above it.
    renderAt('/health');
    expect(screen.getByText(t.health.metricsHeading)).toBeInTheDocument();
  });

  it('renders /shares without crashing', () => {
    // No seeded query data, so Shares renders its loading placeholder rather
    // than any of its real states.
    renderAt('/shares');
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
  });

  it('renders /jobs/42 without crashing', () => {
    // With no seeded query data, JobDetail falls through all three header
    // tiers (no live job, no cached detail) to its loading placeholder — the
    // same text also appears in the attempt-history and events placeholders
    // below it, so assert at least one match rather than a unique heading.
    renderAt('/jobs/42');
    expect(screen.getAllByText(t.query.loading).length).toBeGreaterThan(0);
  });

  it('marks the matching nav item active', () => {
    renderAt('/jobs');
    // CSS Modules hash class names, so match by substring rather than exact class.
    expect(screen.getByRole('link', { name: t.nav.jobs }).className).toMatch(/itemActive/);
  });

  it('gives the route exactly one <h1>, named after the matching nav entry', () => {
    // Events and Peers use neither PageHeading nor SectionHeader for a page
    // title — their ARIA tables above are unnamed too — so this <h1> is the
    // only heading either view has. Layout derives it from the same nav
    // definition the sidebar renders, so it can't drift from the link label.
    renderAt('/peers');
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
    expect(screen.getByRole('heading', { level: 1, name: t.nav.peers })).toBeInTheDocument();
  });

  it('names the nested job-detail route after its parent nav entry', () => {
    renderAt('/jobs/42');
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
    expect(screen.getByRole('heading', { level: 1, name: t.nav.jobs })).toBeInTheDocument();
  });
});
