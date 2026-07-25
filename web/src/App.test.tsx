import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
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
  // '/' (Overview) is asserted separately below: the TUI reskin (#198) gives
  // it no <h1> — PageHeading is gone from that route already, ahead of the
  // other routes below losing theirs in their own tasks.
  it.each([
    ['/jobs', t.nav.jobs],
    // JobDetail with no seeded query data falls through all three header
    // tiers (no live job, no cached detail) to the loading heading.
    ['/jobs/42', t.jobs.loading],
    ['/events', t.nav.events],
    ['/peers', t.nav.peers],
    // No seeded query data, so Shares renders its heading-only loading state.
    ['/shares', t.nav.shares],
    ['/health', t.nav.health],
    ['/settings', t.nav.settings],
  ])('renders %s without crashing', (path, heading) => {
    renderAt(path);
    expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument();
  });

  it('renders / (Overview) without crashing', () => {
    renderAt('/');
    expect(screen.getByText(t.overview.transfersHeading)).toBeInTheDocument();
  });

  it('marks the matching nav item active', () => {
    renderAt('/jobs');
    // CSS Modules hash class names, so match by substring rather than exact class.
    expect(screen.getByRole('link', { name: t.nav.jobs }).className).toMatch(/itemActive/);
  });
});
