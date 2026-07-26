import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { ApiError, apiDelete, apiGet, apiPost, apiPostJson } from './client';
import type {
  AppConfig,
  ChartsReport,
  ConfigUpdateRequest,
  ConfigUpdateResult,
  ConnectionTestResult,
  Conversation,
  Job,
  JobDetail,
  JobEvent,
  MarkReadResult,
  Peer,
  PrivateMessage,
  ShareRescanResult,
  SharesReport,
  StatusReport,
  ThreadPage,
  UploadsReport,
} from './types';

// Intervals match the legacy dashboard exactly so perceived freshness is
// unchanged by the migration.
const JOBS_INTERVAL = 3000;
const EVENTS_INTERVAL = 3000;
// Exported because the top bar derives its staleness threshold from it: a
// hardcoded copy there would silently stop matching if this changed.
export const STATUS_INTERVAL = 5000;
const PEERS_INTERVAL = 5000;
const CHARTS_INTERVAL = 15000; // passes change at most every discovery tick (~30s)
const SHARES_INTERVAL = 15000;
const SHARES_SCANNING_INTERVAL = 3000; // matches JOBS_INTERVAL while a scan is actively running
// Uploads are live transfers, comparable to the jobs list rather than the
// mostly-static share index: a typical track finishes inside 15s (the
// SHARES_INTERVAL), which would miss most uploads' active window entirely.
// 3s matches JOBS_INTERVAL, which polls a similarly-lived array.
const UPLOADS_INTERVAL = 3000;
// Layout mounts useConversations on every route (it feeds the sidebar's chat
// badge), so this matches STATUS_INTERVAL's profile rather than the more
// frequent per-thread interval below.
const CONVERSATIONS_INTERVAL = 5000;
// Only mounted while /chat is on screen, so it can afford to be tighter than
// CONVERSATIONS_INTERVAL: a chat view lagging behind the peer by 5s reads as
// broken in a way a sidebar badge lagging that much does not.
const THREAD_INTERVAL = 3000;

export const queryKeys = {
  jobs: ['jobs'] as const,
  status: ['status'] as const,
  events: ['events'] as const,
  peers: ['peers'] as const,
  config: ['config'] as const,
  charts: ['charts'] as const,
  shares: ['shares'] as const,
  uploads: ['uploads'] as const,
  jobDetail: (id: number) => ['jobs', id, 'detail'] as const,
  jobEvents: (id: number) => ['jobs', id, 'events'] as const,
  // Deliberately not nested under `conversations` — see useMarkConversationRead:
  // marking a thread read must be able to invalidate the conversations list
  // (to refresh its unread badge) without also invalidating every open
  // thread's own message pages.
  conversations: ['messages', 'conversations'] as const,
  thread: (username: string) => ['messages', 'thread', username] as const,
};

export function useJobs() {
  return useQuery({
    queryKey: queryKeys.jobs,
    queryFn: () => apiGet<Job[]>('/api/jobs'),
    refetchInterval: JOBS_INTERVAL,
  });
}

export function useStatus() {
  return useQuery({
    queryKey: queryKeys.status,
    queryFn: () => apiGet<StatusReport>('/status'),
    refetchInterval: STATUS_INTERVAL,
  });
}

export function useEvents() {
  return useQuery({
    queryKey: queryKeys.events,
    queryFn: () => apiGet<JobEvent[]>('/api/events?limit=200'),
    refetchInterval: EVENTS_INTERVAL,
  });
}

export function usePeers() {
  return useQuery({
    queryKey: queryKeys.peers,
    queryFn: () => apiGet<Peer[]>('/api/peers'),
    refetchInterval: PEERS_INTERVAL,
  });
}

// Query keys include the job id, so a slow response for a previously viewed job
// can never overwrite the current one — this replaces the legacy dashboard's
// manual `detailJobId === id` guard.
//
// No `enabled` flag is needed here: the Jobs list expansion panel (issue #60)
// only mounts JobExpansion — and therefore only calls this hook — once a row
// is expanded, so the fetch is naturally scoped to expanded rows rather than
// firing for every row up front.
export function useJobDetail(id: number) {
  return useQuery({
    queryKey: queryKeys.jobDetail(id),
    queryFn: () => apiGet<JobDetail>(`/api/jobs/${id}/detail`),
    refetchInterval: JOBS_INTERVAL,
  });
}

export function useJobEvents(id: number) {
  return useQuery({
    queryKey: queryKeys.jobEvents(id),
    queryFn: () => apiGet<JobEvent[]>(`/api/jobs/${id}/events`),
    refetchInterval: JOBS_INTERVAL,
  });
}

export function useCharts() {
  return useQuery({
    queryKey: queryKeys.charts,
    queryFn: () => apiGet<ChartsReport>('/api/charts'),
    refetchInterval: CHARTS_INTERVAL,
  });
}

export function useConfig() {
  return useQuery({
    queryKey: queryKeys.config,
    queryFn: () => apiGet<AppConfig>('/api/config'),
    staleTime: Infinity, // config only changes when the file changes
  });
}

// The connection test is a one-shot action (not cached state), so it's a
// mutation. It has no side effects on server state, so nothing is invalidated;
// the button reads mutation status directly for its four display states.
export function useTestConnection(dependency: 'lidarr' | 'soulseek') {
  return useMutation({
    mutationFn: () =>
      apiPostJson<ConnectionTestResult>(`/api/config/test/${dependency}`),
  });
}

// A successful update restarts the process to apply it, so the cached config
// is stale until that restart completes — the settings view invalidates
// queryKeys.config itself once its post-save poll of GET /api/config
// succeeds, rather than here.
export function useUpdateConfig() {
  return useMutation({
    mutationFn: (body: ConfigUpdateRequest) =>
      apiPostJson<ConfigUpdateResult>('/api/config', body),
  });
}

export function useCancelJob(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost(`/api/jobs/${id}/cancel`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
      void qc.invalidateQueries({ queryKey: queryKeys.jobDetail(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.jobEvents(id) });
    },
  });
}

// refetchInterval takes the Query object under TanStack Query v5 (not v4's
// (data, query) tuple), so scanning is read off query.state.data.
export function useShares() {
  return useQuery({
    queryKey: queryKeys.shares,
    queryFn: () => apiGet<SharesReport>('/api/shares'),
    refetchInterval: (query) => (query.state.data?.scanning ? SHARES_SCANNING_INTERVAL : SHARES_INTERVAL),
  });
}

// Invalidates onSettled rather than onSuccess (contrast useCancelJob above):
// a 409 conflict response still means a scan is genuinely running server-side,
// so refetching queryKeys.shares is the right reaction to that failure too,
// not just to a successful 202.
export function useRescanShares() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPostJson<ShareRescanResult>('/api/shares/rescan'),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.shares });
    },
  });
}

// Unlike useShares there is no scanning/idle distinction to branch the
// interval on, so a plain constant refetchInterval is enough.
export function useUploads() {
  return useQuery({
    queryKey: queryKeys.uploads,
    queryFn: () => apiGet<UploadsReport>('/api/uploads'),
    refetchInterval: UPLOADS_INTERVAL,
  });
}

export function useRetryJob(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost(`/api/jobs/${id}/retry`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
      // Retry wipes candidate history server-side, so the cached detail is stale.
      void qc.invalidateQueries({ queryKey: queryKeys.jobDetail(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.jobEvents(id) });
    },
  });
}

// 409 unless the job is currently active (see internal/observ POST
// /api/jobs/{id}/search) — mirrors useRetryJob/useCancelJob's invalidation set.
export function useForceSearchJob(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost(`/api/jobs/${id}/search`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
      void qc.invalidateQueries({ queryKey: queryKeys.jobDetail(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.jobEvents(id) });
    },
  });
}

// A hard delete, unlike cancel/retry: the job is gone server-side, so its
// detail/events caches are removed outright rather than just invalidated —
// there's nothing left to refetch.
export function useDeleteJob(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiDelete(`/api/jobs/${id}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
      qc.removeQueries({ queryKey: queryKeys.jobDetail(id) });
      qc.removeQueries({ queryKey: queryKeys.jobEvents(id) });
    },
  });
}

// GET /api/messages answers 503 when private messaging is not enabled in the
// configuration (a plain slskd install, most of the time) — that will never
// start succeeding without a config change and a restart, so polling it
// every CONVERSATIONS_INTERVAL from every route via Layout would otherwise
// log a 503 forever. refetchInterval as a function (not a constant) lets it
// inspect the query's own last error and stop once that specific failure is
// seen, while still retrying normally for any other kind of error (a
// transient 500, a dropped connection) that might recover on its own.
export function useConversations() {
  return useQuery({
    queryKey: queryKeys.conversations,
    queryFn: () => apiGet<Conversation[]>('/api/messages'),
    refetchInterval: (query) => {
      const err = query.state.error;
      if (err instanceof ApiError && err.status === 503) return false;
      return CONVERSATIONS_INTERVAL;
    },
  });
}

// enabled: username !== undefined covers /chat with no thread selected yet
// (before the redirect effect in Chat.tsx picks one). initialPageParam 0
// means "no before= cursor" (beforeID 0 in ThreadFunc); every later page's
// param is the previous page's oldest message id, i.e. its LAST element,
// since the API serves each page newest-first. getNextPageParam returns
// undefined (no more pages) once the server says hasMore is false, or once a
// page comes back empty despite claiming hasMore, which would otherwise loop
// forever requesting the same empty page.
export function useThread(username: string | undefined) {
  // queryFn is never actually invoked while username is undefined (enabled
  // below is false), so this fallback only satisfies the query key/URL
  // builders' string type.
  const key = username ?? '';
  return useInfiniteQuery({
    queryKey: queryKeys.thread(key),
    queryFn: ({ pageParam }) => apiGet<ThreadPage>(`/api/messages/${encodeURIComponent(key)}?before=${pageParam}`),
    enabled: username !== undefined,
    initialPageParam: 0,
    getNextPageParam: (lastPage) =>
      lastPage.hasMore && lastPage.messages.length > 0
        ? lastPage.messages[lastPage.messages.length - 1].id
        : undefined,
    refetchInterval: THREAD_INTERVAL,
  });
}

// The 201 body is discarded on purpose: invalidating the thread and
// conversations queries already refetches both with the new message in
// place, in the correct newest-first order and paging state — hand-splicing
// the mutation's own response into the cache would just duplicate that work
// and risks getting the ordering wrong.
export function useSendMessage(username: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: string) =>
      apiPostJson<PrivateMessage>(`/api/messages/${encodeURIComponent(username)}`, { body }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.thread(username) });
      void qc.invalidateQueries({ queryKey: queryKeys.conversations });
    },
  });
}

// username is the mutation *variable*, not baked into the hook, so one
// instance (called once in Chat.tsx) serves every thread the user opens
// rather than needing a fresh hook per username. Invalidates only
// conversations: the thread view never renders a message's `read` field, so
// there is nothing in queryKeys.thread for this to usefully refresh.
export function useMarkConversationRead() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (username: string) =>
      apiPostJson<MarkReadResult>(`/api/messages/${encodeURIComponent(username)}/read`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.conversations });
    },
  });
}
