import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { AuthGate } from './App';
import Layout from './components/Layout';
import Overview from './routes/Overview';
import Jobs from './routes/Jobs';
import JobDetail from './routes/JobDetail';
import Events from './routes/Events';
import Peers from './routes/Peers';
import Shares from './routes/Shares';
import Health from './routes/Health';
import Settings from './routes/Settings';
import Chat from './routes/Chat';
import { authQueryKeys } from './api/auth';
import { queryKeys } from './api/queries';
import type { Conversation, SessionResponse } from './api/types';
import { t } from './strings';

// Renders the real route tree (mirroring App.tsx) at each path with a
// MemoryRouter, proving every one of the nine routes mounts without
// crashing. BrowserRouter itself is exercised implicitly by the build
// (the SPA is served by the Go backend, which handles history fallback).
function renderAt(path: string, queryClient: QueryClient = new QueryClient()) {
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
            <Route path="chat/:username?" element={<Chat />} />
            <Route path="settings" element={<Settings />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('route tree', () => {
  // Every route below is asserted on stable visible text. Each route renders
  // its own <h1> — via <Page> (#281) for every top-level route, or a
  // Layout-synthesized one for JobDetail (see the dedicated heading tests
  // further down) — so these checks focus on each route's own content
  // instead of duplicating that coverage per route.
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

  it('gives the route exactly one <h1>, from its own <Page> title', () => {
    // Every top-level route (#281) renders its own visible <h1> via <Page>
    // now, rather than a hidden one Layout used to synthesize from the nav
    // label — so this asserts the <Page> title instead of t.nav.peers.
    renderAt('/peers');
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
    expect(screen.getByRole('heading', { level: 1, name: t.page.peers.title })).toBeInTheDocument();
  });

  it('names the nested job-detail route after its parent nav entry', () => {
    renderAt('/jobs/42');
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
    expect(screen.getByRole('heading', { level: 1, name: t.nav.jobs })).toBeInTheDocument();
  });

  it('renders /chat without crashing', () => {
    // No seeded query data, so useConversations() never resolves and Chat
    // renders only its loading state.
    renderAt('/chat');
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
  });

  it('shows the chat nav badge as the sum of unread across conversations', () => {
    const queryClient = new QueryClient();
    const conversations: Conversation[] = [
      { username: 'alice', lastMessage: 'hi', lastMessageAt: '', lastDirection: 'IN', unread: 2, total: 5 },
      { username: 'bob', lastMessage: 'yo', lastMessageAt: '', lastDirection: 'OUT', unread: 1, total: 3 },
    ];
    queryClient.setQueryData(queryKeys.conversations, conversations);
    renderAt('/jobs', queryClient);
    // The badge digit is a second span inside the same <a>, so its accessible
    // name is "chat3", not "chat" — match by prefix rather than exact string.
    expect(screen.getByRole('link', { name: new RegExp(`^${t.nav.chat}`) })).toHaveTextContent('3');
  });
});

// AuthGate (issue #279) sits above the route tree above — the two suites are
// independent, so this one drives AuthGate directly with its own minimal
// children rather than the real route tree.
describe('AuthGate', () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderGate(session?: SessionResponse) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    if (session) client.setQueryData(authQueryKeys.session, session);
    return render(
      <QueryClientProvider client={client}>
        <AuthGate>
          <div>protected content</div>
        </AuthGate>
      </QueryClientProvider>,
    );
  }

  it('renders nothing while the boot-time session check is in flight — no spinner, no flash of the login form', () => {
    // Stubbed to never resolve, so the component stays in its initial
    // isLoading state for the whole test.
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    const { container } = renderGate();
    expect(container).toBeEmptyDOMElement();
  });

  it('renders the first-run setup card when no account exists yet', () => {
    renderGate({ authenticated: false, username: null, setupRequired: true });
    expect(screen.getByRole('heading', { name: t.auth.setupHeader })).toBeInTheDocument();
    expect(screen.queryByText('protected content')).not.toBeInTheDocument();
  });

  // setupRequired wins even when the request is already authenticated. This is
  // NOT the rare curl case it looks like: every install that used the pre-#279
  // native Basic prompt has that credential cached in the browser and replays
  // it automatically, so ordering these the other way round means an upgrading
  // operator never sees the account-creation screen at all. Do not "simplify"
  // by checking authenticated first.
  it('prefers the setup card over the app when a credential is present but no account exists yet', () => {
    renderGate({ authenticated: true, username: null, setupRequired: true });
    expect(screen.getByRole('heading', { name: t.auth.setupHeader })).toBeInTheDocument();
    expect(screen.queryByText('protected content')).not.toBeInTheDocument();
  });

  it('renders the login card once an account exists but this request is unauthenticated', () => {
    renderGate({ authenticated: false, username: null, setupRequired: false });
    expect(screen.getByRole('heading', { name: t.auth.loginHeader })).toBeInTheDocument();
    expect(screen.queryByText('protected content')).not.toBeInTheDocument();
  });

  it('renders the app once authenticated with an account already set up', () => {
    renderGate({ authenticated: true, username: 'sam', setupRequired: false });
    expect(screen.getByText('protected content')).toBeInTheDocument();
  });
});
