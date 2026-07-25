import { QueryClient, QueryClientProvider, keepPreviousData } from '@tanstack/react-query';
import { BrowserRouter, Route, Routes } from 'react-router-dom';
import Layout from './components/Layout';
import Overview from './routes/Overview';
import Jobs from './routes/Jobs';
import JobDetail from './routes/JobDetail';
import Events from './routes/Events';
import Peers from './routes/Peers';
import Shares from './routes/Shares';
import Health from './routes/Health';
import Settings from './routes/Settings';

// The legacy dashboard never blanked the UI on a failed poll — it kept showing
// the last good response. `data` already survives a failed background refetch
// in TanStack Query (the query reducer's error case never touches `data`), and
// `placeholderData: keepPreviousData` extends the same guarantee across a
// query-key change (e.g. navigating between job ids) so no view flashes empty.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      placeholderData: keepPreviousData,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
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
      </BrowserRouter>
    </QueryClientProvider>
  );
}
