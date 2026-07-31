import { QueryCache, QueryClient, QueryClientProvider, keepPreviousData } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { BrowserRouter, Route, Routes } from 'react-router-dom';
import { authQueryKeys, useSession } from './api/auth';
import { ApiError } from './api/client';
import { StreamProvider } from './api/stream';
import Layout from './components/Layout';
import Login from './routes/Login';
import Overview from './routes/Overview';
import Jobs from './routes/Jobs';
import JobDetail from './routes/JobDetail';
import Events from './routes/Events';
import Peers from './routes/Peers';
import Shares from './routes/Shares';
import Health from './routes/Health';
import Settings from './routes/Settings';
import Search from './routes/Search';
import Chat from './routes/Chat';
import Setup from './routes/Setup';

// The legacy dashboard never blanked the UI on a failed poll — it kept showing
// the last good response. `data` already survives a failed background refetch
// in TanStack Query (the query reducer's error case never touches `data`), and
// `placeholderData: keepPreviousData` extends the same guarantee across a
// query-key change (e.g. navigating between job ids) so no view flashes empty.
const queryClient = new QueryClient({
  // Global: any query that comes back 401 (a session that expired or was
  // revoked mid-visit, issue #279) refetches the session query, which flips
  // AuthGate back to the login card. This is the one cross-cutting reaction
  // to 401 the whole app needs — every individual query keeps its own error
  // handling for everything else.
  queryCache: new QueryCache({
    onError: (error) => {
      if (error instanceof ApiError && error.status === 401) {
        void queryClient.invalidateQueries({ queryKey: authQueryKeys.session });
      }
    },
  }),
  defaultOptions: {
    queries: {
      placeholderData: keepPreviousData,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

// Gates the whole app behind GET /api/auth/session (issue #279): renders
// nothing while that boot-time check is in flight (no spinner, no flash of
// the login form), then either `children` unchanged or the login/first-run
// card in its place. `setupRequired` wins over `authenticated`, and that order
// matters: an install with no account is not finished, whoever is asking. The
// combination authenticated:true + setupRequired:true is not the rare curl case
// it looks like — every install that used the pre-#279 native Basic prompt has
// the token cached in the browser, which replays it automatically, so ordering
// it the other way round means an upgrading operator is never shown the
// account-creation screen and the login they just deployed silently does
// nothing. The cost of this order is that `make dev` against a freshly reset
// lab asks for an account each time; that is the cheaper mistake.
export function AuthGate({ children }: { children: ReactNode }) {
  const session = useSession();
  if (session.isLoading) return null;
  if (session.data?.setupRequired) return <Login mode="setup" />;
  if (session.data?.authenticated) return <>{children}</>;
  return <Login mode="login" />;
}

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        {/* StreamProvider sits INSIDE the gate: it opens /api/stream, a private
            endpoint, so mounting it above AuthGate made the login screen fire a
            request that 401s before the user has any way to authenticate. */}
        <AuthGate>
          <StreamProvider>
            <Routes>
              <Route element={<Layout />}>
                <Route index element={<Overview />} />
                <Route path="jobs" element={<Jobs />} />
                <Route path="jobs/:id" element={<JobDetail />} />
                <Route path="events" element={<Events />} />
                <Route path="peers" element={<Peers />} />
                <Route path="shares" element={<Shares />} />
                <Route path="health" element={<Health />} />
                <Route path="search" element={<Search />} />
                <Route path="chat/:username?" element={<Chat />} />
                <Route path="setup" element={<Setup />} />
                <Route path="settings" element={<Settings />} />
              </Route>
            </Routes>
          </StreamProvider>
        </AuthGate>
      </BrowserRouter>
    </QueryClientProvider>
  );
}
