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
import Health from './routes/Health';
import Settings from './routes/Settings';

// Renders the real route tree (mirroring App.tsx) at each path with a
// MemoryRouter, proving every one of the seven routes mounts without
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
            <Route path="health" element={<Health />} />
            <Route path="settings" element={<Settings />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('route tree', () => {
  it.each([
    ['/', 'Overview'],
    ['/jobs', 'Jobs'],
    ['/jobs/42', 'Job detail'],
    ['/events', 'Events'],
    ['/peers', 'Peers'],
    ['/health', 'Health'],
    ['/settings', 'Settings'],
  ])('renders %s without crashing', (path, heading) => {
    renderAt(path);
    expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument();
  });

  it('marks the matching nav item active', () => {
    renderAt('/jobs');
    // CSS Modules hash class names, so match by substring rather than exact class.
    expect(screen.getByRole('link', { name: 'Jobs' }).className).toMatch(/navItemActive/);
  });
});
