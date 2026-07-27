import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { ApiError, apiDelete, apiGet, apiPost, apiPostJson } from './client';
import { normalizeJobDetail, normalizeJobPage, normalizeJobs, normalizeStatusReport } from './normalize';
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
  JobPage,
  JobPageParams,
  LiveJob,
  MarkReadResult,
  Peer,
  PrivateMessage,
  ScopedLivePayload,
  ShareRescanResult,
  SharesReport,
  ThreadPage,
  ThroughputSample,
  UploadsReport,
  WireJob,
  WireJobDetail,
  WireJobPage,
  WireStatusReport,
} from './types';

// jobs was 3s before the SSE stream (#161) carried its live fields at ~1s
// instead; it now only needs to be fresh enough for the DB-backed fields
// (status/state/events-derived data), which change on a pipeline tick, not
// continuously. See the #161 design doc's poll-interval table.
const JOBS_INTERVAL = 15000;
// Job detail deliberately keeps the old 3s cadence rather than following
// JOBS_INTERVAL. The stream only carries a detail for the job its connection
// was opened with (/api/stream?job=<id>, set on the /jobs/:id route), but
// JobExpansion renders the same per-file transfers inline on the /jobs list,
// where the connection is unscoped and REST is the only source. Slowing this
// to 15s would make an expanded row's file progress update 5x slower than
// before #161 — a regression the stream does not compensate for on that
// route. The #161 design's poll-interval table changes `jobs`, not the
// detail query.
//
// On /jobs/:id the poll is not redundant either: it keeps this cache key
// warm as the fallback the moment the stream drops, and it stays the only
// *writer* of the key — pickJobDetail chooses between the two objects at
// read time rather than writing the stream's into the cache, so there is
// never more than one author of the cached value (issue #258).
const JOB_DETAIL_INTERVAL = 3000;
const EVENTS_INTERVAL = 3000;
// Exported because the top bar derives its staleness threshold from it: a
// hardcoded copy there would silently stop matching if this changed.
export const STATUS_INTERVAL = 5000;
const PEERS_INTERVAL = 5000;
const CHARTS_INTERVAL = 15000; // passes change at most every discovery tick (~30s)
const SHARES_INTERVAL = 15000;
const SHARES_SCANNING_INTERVAL = 3000; // a scan's progress is worth watching closely while it runs
// Uploads are live transfers, comparable to the jobs list rather than the
// mostly-static share index: a typical track finishes inside 15s (the
// SHARES_INTERVAL), which would miss most uploads' active window entirely.
// 3s matches JOB_DETAIL_INTERVAL, which polls a similarly-lived array.
// (Uploads are not on the #161 stream — it carries downloads only.)
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
  jobsPage: (params: JobPageParams) => [
    'jobs',
    'page',
    params.page,
    params.sort,
    params.dir,
    params.filter,
    params.source,
    params.q,
  ] as const,
  jobsAll: ['jobs', 'all'] as const,
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
  // Not backed by a queryFn — see useLiveData. api/stream.ts's StreamProvider
  // is the only writer, via setQueryData on every `event: live` frame (and
  // clears it to null on stream error/close — null rather than undefined
  // because setQueryData ignores undefined; see the comment there).
  live: ['live'] as const,
};

// Reads the SSE stream's latest frame (see api/stream.ts's StreamProvider)
// without ever fetching it itself: queryFn is never invoked because
// `enabled` is false, but the query still subscribes to cache updates, so a
// component re-renders whenever StreamProvider calls
// queryClient.setQueryData(queryKeys.live, ...). undefined means either no
// frame has arrived yet or the stream just died — both cases the merge
// functions below treat identically ("nothing live right now").
export function useLiveData(): ScopedLivePayload | null | undefined {
  return useQuery<ScopedLivePayload | null>({
    queryKey: queryKeys.live,
    queryFn: () => null,
    enabled: false,
    staleTime: Infinity,
  }).data;
}

// Overlays a job's live fields on top of its REST values — never a
// destructive replace. A job absent from `live` (not currently reporting a
// matched, non-terminal live transfer — see LivePayload's doc comment) is
// returned untouched, which is exactly "fall back to REST" since `jobs`
// already carries whatever REST last reported for it.
export function mergeLiveJobs(jobs: Job[] | undefined, live: LiveJob[] | undefined): Job[] | undefined {
  if (!jobs || !live || live.length === 0) return jobs;
  const byId = new Map(live.map((j) => [j.id, j]));
  return jobs.map((job) => {
    const l = byId.get(job.id);
    if (!l) return job;
    return {
      ...job,
      bytesDone: l.bytesDone,
      bytesTotal: l.bytesTotal,
      speed: l.speed,
      queuePosition: l.queuePosition,
      etaSeconds: l.etaSeconds,
    };
  });
}

// A page is an ordered server result, so live data may update fields only.
// It must never splice stream-only IDs into the page or disturb its metadata.
export function mergeLiveJobPage(page: JobPage | undefined, live: LiveJob[] | undefined): JobPage | undefined {
  if (!page) return page;
  const jobs = mergeLiveJobs(page.jobs, live);
  return jobs === page.jobs ? page : { ...page, jobs: jobs! };
}

// Picks the job detail to render: the stream's when it carries one for this
// job, otherwise REST's.
//
// This is a *replacement*, not a merge, and that is the whole point of issue
// #258. Both objects are produced by the same server-side function
// (internal/observ's toJobDetailDTO, called by the REST handler and by the
// stream hub alike), so neither is a partial view the other has to complete.
// The previous version overlaid the two field by field on the client, which
// cannot be made correct: live leads REST on bytesDone and on a transfer
// reaching a terminal state, by up to a whole downloading_interval (15s),
// while REST is the only source of retries, lastProgressAt and attempt state
// — and in the instant between reconcile's commit and its purge the ordering
// flips again. Four separate regressions in #161 came from that merge.
//
// The scope check stays: a frame carries `detail` only for the job its
// connection was opened with, so a stale frame arriving mid-reconnect after
// navigating between jobs must never be adopted by the job now on screen.
export function pickJobDetail(detail: JobDetail | undefined, live: ScopedLivePayload | null | undefined, id: number): JobDetail | undefined {
  if (!live || live.scopeJobId !== id || !live.detail) return detail;
  return live.detail;
}

// Matches internal/soulseek's throughputWindow (48 one-second samples) —
// the server itself never reports more than that per direction in one
// GET /api/charts snapshot, so capping each client-merged series to the same
// size mirrors what REST would eventually re-converge to anyway rather than
// growing either series unbounded as the stream keeps appending.
const THROUGHPUT_CAP = 48;

// Appends one direction's new throughput samples to its already-fetched
// series, deduping on `at` (the REST snapshot and a live frame can legitimately
// overlap on the sample taken right at fetch time) and capping the result —
// see THROUGHPUT_CAP. Call independently for downloads and uploads so one
// direction can neither dedupe nor evict the other. Oldest-first in,
// oldest-first out, matching both REST's and the stream's own ordering.
export function mergeThroughputSamples(existing: ThroughputSample[], incoming: ThroughputSample[]): ThroughputSample[] {
  if (incoming.length === 0) return existing;
  const seen = new Set(existing.map((s) => s.at));
  const deduped = incoming.filter((s) => !seen.has(s.at));
  if (deduped.length === 0) return existing;
  return [...existing, ...deduped].slice(-THROUGHPUT_CAP);
}

export const JOBS_PAGE_SIZE = 12;

export const DEFAULT_JOB_PAGE_PARAMS: JobPageParams = {
  page: 0,
  sort: 'st',
  dir: 'asc',
  filter: 'all',
  source: 'all',
  q: '',
};

export function jobsPageUrl(params: JobPageParams): string {
  const query = new URLSearchParams();
  query.set('page', String(params.page));
  query.set('sort', params.sort);
  query.set('dir', params.dir);
  query.set('filter', params.filter);
  query.set('source', params.source);
  query.set('q', params.q);
  return `/api/jobs?${query.toString()}`;
}

export function useJobs(params: JobPageParams) {
  const jobsQuery = useQuery({
    queryKey: queryKeys.jobsPage(params),
    queryFn: () => apiGet<WireJobPage>(jobsPageUrl(params)).then(normalizeJobPage),
    refetchInterval: JOBS_INTERVAL,
  });
  const live = useLiveData();
  return { ...jobsQuery, data: mergeLiveJobPage(jobsQuery.data, live?.jobs) };
}

// Only consumers that truly need the complete collection should use this.
// The paged Jobs route must stay on useJobs so filtering and facets remain
// globally correct on the server.
export function useAllJobs() {
  const jobsQuery = useQuery({
    queryKey: queryKeys.jobsAll,
    queryFn: () => apiGet<WireJob[]>('/api/jobs/all').then(normalizeJobs),
    refetchInterval: JOBS_INTERVAL,
  });
  const live = useLiveData();
  return { ...jobsQuery, data: mergeLiveJobs(jobsQuery.data, live?.jobs) };
}

export function useStatus() {
  return useQuery({
    queryKey: queryKeys.status,
    queryFn: () => apiGet<WireStatusReport>('/status').then(normalizeStatusReport),
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
  const detailQuery = useQuery({
    queryKey: queryKeys.jobDetail(id),
    queryFn: () => apiGet<WireJobDetail>(`/api/jobs/${id}/detail`).then(normalizeJobDetail),
    refetchInterval: JOB_DETAIL_INTERVAL,
  });
  const live = useLiveData();
  return { ...detailQuery, data: pickJobDetail(detailQuery.data, live, id) };
}

export function useJobEvents(id: number) {
  return useQuery({
    queryKey: queryKeys.jobEvents(id),
    queryFn: () => apiGet<JobEvent[]>(`/api/jobs/${id}/events`),
    refetchInterval: JOB_DETAIL_INTERVAL,
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

// Username travels with the body as a mutation variable so a send paused
// before its POST cannot be retargeted by a rerender. The 201 body is
// discarded on purpose: invalidating the thread and conversations queries
// already refetches both with the new message in place, in the correct
// newest-first order and paging state — hand-splicing the mutation's own
// response into the cache would just duplicate that work and risks getting
// the ordering wrong.
export function useSendMessage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ username, body }: { username: string; body: string }) =>
      apiPostJson<PrivateMessage>(`/api/messages/${encodeURIComponent(username)}`, { body }),
    onSuccess: (_message, { username }) => {
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
