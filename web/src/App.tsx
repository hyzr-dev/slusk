import { QueryClient, QueryClientProvider, keepPreviousData } from '@tanstack/react-query';

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
      <div>slskdarr</div>
    </QueryClientProvider>
  );
}
