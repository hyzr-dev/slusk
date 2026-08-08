import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { ApiError, apiDelete, apiGet, apiPost, apiPostJson } from './client';
import {
  normalizeJob,
  normalizeJobDetail,
  normalizeJobPage,
  normalizeSearchGroup,
  normalizeSearchSession,
  normalizeStatusReport,
} from './normalize';
import type {
  AddLidarrArtistRequest,
  AddLidarrArtistResult,
  AppConfig,
  BulkRetryResult,
  ChartsReport,
  ConfigUpdateRequest,
  ConfigUpdateResult,
  ConnectionTestResult,
  Conversation,
  CreateJobRequest,
  Job,
  JobDetail,
  JobEvent,
  JobPage,
  JobPageParams,
  JobStatusFilter,
  LidarrAddOptions,
  LidarrArtistMatch,
  LidarrMatch,
  MarkReadResult,
  MusicBrainzEditionListResult,
  MusicBrainzSearchResponse,
  PeerHistory,
  PeerPage,
  PeerPageParams,
  PrivateMessage,
  ScopedLivePayload,
  SearchPayload,
  SearchSession,
  ShareRescanResult,
  SharesReport,
  ThreadPage,
  ThroughputSample,
  UploadHistoryPage,
  UploadsReport,
  WireJob,
  WireJobDetail,
  WireJobPage,
  WireSearchSession,
  WireStatusReport,
} from './types';

// jobs was 3s before the SSE stream (#161) carried its live fields at ~1s
// instead, then 15s once the DB-backed fields (status/state/events-derived
// data) were the only thing left to poll for. Issue #275 replaced that fixed
// timer as the primary freshness mechanism with the stream's own `event:
// invalidate`: the backend fingerprints the jobs table and pushes a signal
// only when a page/facet/sort-relevant field actually changed, so an idle
// system now polls zero times instead of every 15s. This is now a SAFETY
// NET, not the primary mechanism, for two known gaps `event: invalidate`
// cannot close:
//   - a dropped or missed SSE frame (reconnects self-heal via onopen's own
//     invalidateQueries, but a frame lost without a disconnect would not);
//   - `filter=finished`'s window bound (DashboardFinishedWindow, 1h): a job
//     ages out of Overview's RECENTLY FINISHED panel with no database write
//     at all, so no fingerprint change and no invalidation ever fires for
//     it — this poll is the ONLY thing that ever corrects it, which is why
//     this constant can never become `false` as long as that panel exists.
// 60000 (four times streamInvalidateInterval's 15s floor, not equal to it)
// rather than removed entirely is deliberately conservative given those two
// gaps.
const JOBS_INTERVAL = 60000;
// Job detail deliberately keeps the old 3s cadence rather than following
// JOBS_INTERVAL. The stream only carries a detail for the job its connection
// was opened with (/api/stream?job=<id>, set on the /jobs/:id route), but
// JobExpansion renders the same per-file transfers inline on the /jobs list,
// where the connection is scoped to a page of ids and carries no detail at
// all — REST is the only source there. Slowing this to JOBS_INTERVAL (60s)
// would make an expanded row's file progress update 20x slower than before
// #161 — a regression the stream does not compensate for on that route. The
// #161 design's poll-interval table changes `jobs`, not the detail query.
//
// This is the interval for the views the stream does *not* serve. On
// /jobs/:id, where it does, useJobDetail switches the poll off entirely
// (issue #274) — see streamCarriesJobDetail.
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
// How long a streamed job's server-computed framedAt may age before
// replaceLiveJobs stops trusting it and falls back to REST (issue #285).
// Comfortably above internal/observ's streamCorrelationInterval (5s,
// stream.go) — the worst-case gap between two genuine view refreshes for
// a job that's still actually live — and below the invalidate throttle (15s,
// streamInvalidateInterval), so a job that has truly left the live-matched
// set unpins before the next `event: invalidate`-triggered refetch (or
// JOBS_INTERVAL's safety-net poll) would have corrected it anyway.
const LIVE_JOB_FRESH_MS = 10000;
// Fallback poll for a manual search session (issue #58), used only while the
// SSE connection isn't the one keeping it current: a batching backend
// (streaming === false, i.e. slskd — see core.SearchSession's doc comment)
// never gets incremental frames at all, and a genuinely dead connection
// (queryKeys.live === null — the shared EventSource errored) can't deliver
// them either. Matches JOB_DETAIL_INTERVAL's cadence: a search view is
// actively watched, unlike JOBS_INTERVAL's idle-safety-net profile.
const SEARCH_POLL_INTERVAL = 3000;
// How long an open SSE connection may deliver no `event: search` frame at all
// before useSearchSession stops trusting it and arms the poll anyway.
//
// The connection being *up* is not evidence that frames are arriving: a hub
// bug, a dropped sendLatestSearch, or a proxy buffering the response body all
// leave EventSource perfectly healthy while nothing reaches the view — and
// that open-but-silent state is precisely what the REST fallback exists to
// cover. Two poll intervals, so a genuinely sparse but working search (peers
// trickling in) costs at most one redundant GET, which merges losslessly
// anyway (see mergeSearchSession) rather than dropping anything.
const SEARCH_STREAM_STALE_MS = 2 * SEARCH_POLL_INTERVAL;

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
    params.pageSize,
    params.skipFacets,
  ] as const,
  status: ['status'] as const,
  events: ['events'] as const,
  peers: ['peers'] as const,
  // Nested under `peers` so a future invalidateQueries on the list reaches
  // every page. The literal 'page' segment keeps these keys disjoint from
  // peerHistory's ['peers', <username>, 'history'] — a peer named "page"
  // would still not collide, since the third segment differs in type.
  peersPage: (params: PeerPageParams) =>
    ['peers', 'page', params.page, params.sort, params.dir] as const,
  // Deliberately nested under `peers`: one peer's artist history is a strict
  // child of the list, and the list's PEERS_INTERVAL poll invalidating an
  // open expansion alongside it is exactly the wanted behaviour.
  peerHistory: (username: string) => ['peers', username, 'history'] as const,
  config: ['config'] as const,
  charts: ['charts'] as const,
  shares: ['shares'] as const,
  uploads: ['uploads'] as const,
  // Deliberately not nested under `uploads` — see useUploadHistory: a
  // future invalidateQueries({ queryKey: queryKeys.uploads }) from the
  // live-uploads poll has no business discarding paged history the user has
  // already scrolled through.
  uploadHistory: ['uploadHistory'] as const,
  jobDetail: (id: number) => ['jobs', id, 'detail'] as const,
  jobEvents: (id: number) => ['jobs', id, 'events'] as const,
  // Deliberately not nested under `conversations` — see useMarkConversationRead:
  // marking a thread read must be able to invalidate the conversations list
  // (to refresh its unread badge) without also invalidating every open
  // thread's own message pages.
  conversations: ['messages', 'conversations'] as const,
  thread: (username: string) => ['messages', 'thread', username] as const,
  // Not backed by a queryFn — see useLiveData. api/stream.tsx's StreamProvider
  // is the only writer, via setQueryData on every `event: live` frame (and
  // clears it to null on stream error, or when StreamProvider itself
  // unmounts — deliberately NOT when the connection merely reopens with a
  // new scope, see issue #276. null rather than undefined because
  // setQueryData ignores undefined; see the comment there).
  live: ['live'] as const,
  // One manual search session (issue #58). Written by two independent
  // sources sharing this same key — useSearchSession's queryFn (the REST
  // snapshot, and the fallback poll's re-fetch of it) and api/stream.tsx's
  // `search` listener (incremental group deltas, folded in by
  // replaceSearchGroups) — unlike queryKeys.live, there is no separate
  // live-only cache to pick between at read time: both writers produce the
  // same SearchSession shape, so the stream's writes are ordinary
  // setQueryData patches onto the REST-seeded object, not a parallel source.
  search: (id: string) => ['search', id] as const,
};

// Reads the SSE stream's latest frame (see api/stream.tsx's StreamProvider)
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

// Chooses between a job's REST row and its accumulated streamed counterpart
// at read time — never a field-by-field merge (issue #258, the same shape as
// pickJobDetail below: both objects are built by the same server-side
// function, so whichever wins is a complete, self-consistent replacement,
// not a partial view to overlay).
//
// The choice is `framedAt`, not simple presence in `live`: stream.tsx
// accumulates jobs by id and never evicts an entry on its own (see its doc
// comment), because the backend deliberately stops streaming a job once it
// leaves the live-matched set — including the post-terminal DB transitions
// that only settle after the last live frame (e.g. DOWNLOADING -> IMPORTING
// -> COMPLETED once a transfer finishes and reconcile runs). If presence in
// `live` were enough to win, a job would stay pinned at its last live values
// for the remaining lifetime of the connection with nothing left to
// self-heal it: a trailing DOWNLOADING -> IMPORTING -> COMPLETED transition
// moves `state`, `Status` and `updatedAt`, all fingerprinted fields (issue
// #275), so it bumps jobsGeneration and a refetch follows within
// streamInvalidateInterval regardless of framedAt's own staleness window.
//
// An earlier version compared `updatedAt` instead. That measured the wrong
// thing (issue #285): REST's updatedAt and the stream's updatedAt are read
// from two independently-cached copies of the same DB row, with different
// staleness windows (REST: up to 60s stale via React Query, JOBS_INTERVAL's
// safety-net floor; stream: up to 5s stale via the server's own correlation
// cache) — so the comparison
// told you which cache last happened to read the DB, not which side's data
// was actually fresher. A stale streamed row could therefore win forever
// once its job left the live-matched set.
//
// `framedAt` fixes that: it's server-set to the instant the DTO instance was
// computed (see internal/observ's jobDTO.FramedAt), so it directly measures
// how old the streamed object is, independent of either side's cache. The
// streamed row wins only while its framedAt is within LIVE_JOB_FRESH_MS of
// now; once it ages past that, REST wins and the pin releases.
//
// A job absent from `live` entirely (never streamed since the last reset —
// see LivePayload's doc comment) trivially falls back to REST, since there
// is nothing to compare against.
export function replaceLiveJobs(jobs: Job[] | undefined, live: WireJob[] | undefined): Job[] | undefined {
  if (!jobs || !live || live.length === 0) return jobs;
  const byId = new Map(live.map((j) => [j.id, normalizeJob(j)]));
  return jobs.map((job) => {
    const streamed = byId.get(job.id);
    if (!streamed) return job;
    const stale = Date.now() - new Date(streamed.framedAt).getTime() > LIVE_JOB_FRESH_MS;
    return stale ? job : streamed;
  });
}

// Filters whose selection is terminal by construction: every row such a page
// can contain describes a job the pipeline is done with, so a streamed frame —
// which only ever describes an in-flight job — has nothing true to say about
// it and must not be merged in (issue #291).
//
// Membership follows the backend's own predicates in
// internal/store/dashboard.go (dashboardJobsWhere and dashboardJobStatusSQL):
//
//   - 'finished' and 'failures' are state-keyed (DONE/FAILED/NOT_IMPORTED and
//     FAILED respectively) and are Overview's terminal panels.
//   - 'done', 'notImported' and 'parked' are status-derived, but each maps
//     one-to-one onto a terminal j.state (DONE/COMPLETED, NOT_IMPORTED,
//     PARKED/ORPHANED) with no transfer-aggregate fallback.
//   - 'failed' is deliberately absent. It is status-derived and also matches a
//     job still DOWNLOADING whose current candidate's transfers all errored
//     and which the pipeline will retry — that job is genuinely live, and its
//     streamed row is the truthful one.
const TERMINAL_JOB_FILTERS: ReadonlySet<JobStatusFilter> = new Set<JobStatusFilter>([
  'finished',
  'failures',
  'done',
  'notImported',
  'parked',
]);

/**
 * True when a page fetched with this filter holds only jobs the pipeline has
 * finished with, and therefore must not adopt streamed live rows.
 *
 * Derived from the filter rather than taken as a per-caller opt-out on
 * purpose: terminality is a property of the query, so no view can forget to
 * ask for it, and adding one more panel over an existing terminal filter needs
 * no thought about the live merge at all.
 */
export function isTerminalJobFilter(filter: JobStatusFilter): boolean {
  return TERMINAL_JOB_FILTERS.has(filter);
}

// A page is an ordered server result, so live data may replace existing rows
// only. It must never splice stream-only IDs into the page or disturb its
// metadata.
export function replaceLiveJobPage(page: JobPage | undefined, live: WireJob[] | undefined): JobPage | undefined {
  if (!page) return page;
  const jobs = replaceLiveJobs(page.jobs, live);
  return jobs === page.jobs ? page : { ...page, jobs: jobs! };
}

// True when the live stream is currently the authority for this job's
// detail: the connection was opened as /api/stream?job=<id> for this very id
// *and* the frame actually carries a body (buildStreamDetail in
// internal/observ/stream.go omits it until the hub has both a cached detail
// and a job view for the id).
//
// Deliberately the single expression of that condition, shared by the read
// side (pickJobDetail) and the fetch side (useJobDetail's refetchInterval).
// Two copies of it is precisely the drift issue #269 was about, and here the
// two sides must agree exactly: a fetch side that disabled polling under a
// weaker condition than the read side accepts the stream's value would leave
// the view with neither source — frozen on whatever REST last returned.
//
// `live` being null rather than undefined is what makes a dropped stream
// flip this back to false; see StreamProvider's clearLive for why it must be
// null.
function streamCarriesJobDetail(
  live: ScopedLivePayload | null | undefined,
  id: number,
): live is ScopedLivePayload & { detail: JobDetail } {
  return !!live && live.scopeJobId === id && !!live.detail;
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
  if (!streamCarriesJobDetail(live, id)) return detail;
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

// Folds one `event: search` frame into the cached session for its id
// (mirrors replaceLiveJobs' by-id merge, but patches a single session object
// in place rather than picking between two independent caches — see
// queryKeys.search's doc comment for why that's safe here). `prev` is either
// the REST snapshot useSearchSession/useStartSearch already cached, or an
// earlier frame already folded in — both share the same SearchSession shape,
// so this is always a legitimate base to patch onto. A frame for a
// different id than `prev` (a stale frame arriving after the view has moved
// to a new search — the connection reopens on every new search, but a frame
// already in flight when it does could still land) is ignored outright, and
// so is a frame arriving before anything has been fetched yet (`prev`
// undefined) — the imminent REST response will carry the same information.
export function replaceSearchGroups(prev: SearchSession | undefined, frame: SearchPayload): SearchSession | undefined {
  if (!prev || prev.id !== frame.id) return prev;
  if (frame.expired) {
    // The session was evicted server-side between subscribing and this
    // frame (or a reconnect landed on an id that has since finished and
    // aged out) — no more `search` frames will ever arrive for this id.
    // Mark it done so the view stops rendering a "searching…" state, but
    // keep whatever groups are already cached rather than blanking them,
    // and record `expired` so the header can say the session was evicted
    // rather than claiming the search finished (see SearchSession).
    return prev.done && prev.expired ? prev : { ...prev, done: true, expired: true };
  }
  const byId = new Map(prev.groups.map((g) => [g.id, g]));
  for (const g of frame.groups ?? []) byId.set(g.id, normalizeSearchGroup(g));
  return {
    ...prev,
    groups: Array.from(byId.values()),
    total: frame.total,
    done: frame.done,
    streaming: frame.streaming,
    truncated: frame.truncated ?? prev.truncated,
    error: frame.error || prev.error,
    // Local clock, stamped on every frame actually folded in — the freshness
    // signal useSearchSession's fallback poll arms on. See SearchSession.
    streamedAt: Date.now(),
  };
}

// Folds a REST snapshot onto whatever is already cached for the same session,
// rather than replacing it (issue #58 review).
//
// The two writers of queryKeys.search(id) can be live at the same time — the
// stream blips, `onerror` arms the poll, EventSource reconnects on its own,
// and now both are running. A GET computed at T1 that lands after a frame
// folded in at T2 > T1 would, on a plain replace, wipe the groups that frame
// added; the server's per-subscriber cursor has already advanced past them, so
// they are never resent and the cards visibly flicker out mid-search.
//
// Merging by group id rather than splitting the two into separate caches (the
// shape replaceLiveJobs uses) is deliberate: there is no "which source wins"
// question to answer here. A streamed `search` frame is a cumulative delta
// over a cursor, not a competing snapshot of the same row, so union-by-id is
// the correct semantics for BOTH writers and a split would only force this
// accumulation to exist twice. Where both sides hold the same group id the
// one with more files wins — see the loop below. The scalars REST does own are taken from the
// fetched snapshot, except the three that are monotonic over a session's life
// (`done`, `expired`, `truncated`) and `total`, which are OR'd/maxed so a
// stale snapshot cannot un-finish, un-expire or un-truncate the view.
// `streamedAt` is carried over untouched — a REST fetch says nothing about
// when the stream last spoke.
export function mergeSearchSession(prev: SearchSession | undefined, fetched: SearchSession): SearchSession {
  if (!prev || prev.id !== fetched.id) return fetched;
  const byId = new Map(prev.groups.map((g) => [g.id, g]));
  for (const g of fetched.groups) {
    // Union-by-id alone only closes half the race. A snapshot computed at T1
    // that lands after a frame folded in at T2 > T1 still carries T1's copy
    // of a group the frame GREW, and a plain set() reverts it to that shorter
    // file list — the cursor has advanced, so the stream won't resend it, and
    // "Download album" then enqueues an incomplete album with nothing on
    // screen looking wrong. Resolving that by file count is sound because a
    // group's file set is strictly append-only within a session: app/search.go's
    // accept() only ever appends to the accumulator and rebuilds the whole
    // group from that slice, so `files.length` is monotone per id and the
    // longer list is by definition the newer one. (Deliberately not a `version`
    // comparison — seq is not on the wire per group and must not be added.)
    const cached = byId.get(g.id);
    if (cached && cached.files.length > g.files.length) continue;
    byId.set(g.id, g);
  }
  return {
    ...fetched,
    groups: Array.from(byId.values()),
    total: Math.max(fetched.total, prev.total),
    done: fetched.done || prev.done,
    expired: fetched.expired || prev.expired,
    truncated: fetched.truncated || prev.truncated,
    error: fetched.error || prev.error,
    streamedAt: prev.streamedAt,
  };
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
  // Omitted (not defaulted to JOBS_PAGE_SIZE here) so the paged Jobs route's
  // URL is unchanged from before pageSize existed — the backend's own
  // default already matches JOBS_PAGE_SIZE, and only a caller that actually
  // wants a different size (Overview, issue #268) needs to send it.
  if (params.pageSize !== undefined) query.set('pageSize', String(params.pageSize));
  // Only sent when opting out: the server defaults to facets=1, so an absent
  // parameter and facets=1 mean the same thing and omitting it keeps the URL
  // (and therefore the cache key) unchanged for every existing caller.
  if (params.skipFacets) query.set('facets', '0');
  return `/api/jobs?${query.toString()}`;
}

export function useJobs(params: JobPageParams) {
  const jobsQuery = useQuery({
    queryKey: queryKeys.jobsPage(params),
    queryFn: () => apiGet<WireJobPage>(jobsPageUrl(params)).then(normalizeJobPage),
    refetchInterval: JOBS_INTERVAL,
    // Overrides App.tsx's global refetchOnWindowFocus: false (issue #275).
    // That global default exists so a background/blurred tab's failed or
    // skipped poll never blanks the UI; it says nothing about a RETURNING
    // tab. Since stream.tsx's `invalidate` listener now skips refetching
    // entirely while `document.hidden` (to avoid costing a background tab
    // requests an idle SSE connection previously cost zero of), a tab parked
    // past a generation bump would otherwise wait out JOBS_INTERVAL's 60s
    // safety-net floor before catching up. Refetching on focus closes that
    // gap immediately, and this is scoped to just this query rather than
    // flipping the global default, which every other view still relies on.
    refetchOnWindowFocus: true,
  });
  const live = useLiveData();
  // A terminal page is a settled record, so it takes REST's row as-is — see
  // isTerminalJobFilter. The hook still subscribes to the live cache
  // unconditionally (hooks cannot be called conditionally, and the
  // subscription costs nothing but a re-render that yields the same page).
  if (isTerminalJobFilter(params.filter)) return jobsQuery;
  return { ...jobsQuery, data: replaceLiveJobPage(jobsQuery.data, live?.jobs) };
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

// The Peers list's page size. Matches the backend's own default
// (observ.peersPageSize), which is why peersPageUrl does not send it.
export const PEERS_PAGE_SIZE = 25;

export const DEFAULT_PEER_PAGE_PARAMS: PeerPageParams = {
  page: 0,
  sort: 'score',
  dir: 'desc',
};

export function peersPageUrl(params: PeerPageParams): string {
  const query = new URLSearchParams();
  query.set('page', String(params.page));
  query.set('sort', params.sort);
  query.set('dir', params.dir);
  return `/api/peers?${query.toString()}`;
}

/**
 * One page of known peers, ordered server-side (issue #426).
 *
 * The ordering cannot be done here: `score` is a time-decayed sigmoid over the
 * whole set, and sorting the fetched page would be a different claim than the
 * column header makes — rows would silently reorder as the user pages.
 */
export function usePeers(params: PeerPageParams) {
  return useQuery({
    queryKey: queryKeys.peersPage(params),
    queryFn: () => apiGet<PeerPage>(peersPageUrl(params)),
    refetchInterval: PEERS_INTERVAL,
  });
}

/**
 * GET /api/peers/{username} — one peer's per-artist reliability history.
 *
 * `enabled` is what keeps the split worth making (issue #424): the list no
 * longer carries this data, so it must be fetched for the one expanded row and
 * no other. Peers.tsx passes username=null while nothing is expanded.
 *
 * Not polled. Reliability counters move when a transfer finishes, not on a
 * timer, and the row is open only as long as someone is reading it.
 */
export function usePeerHistory(username: string | null) {
  return useQuery({
    queryKey: queryKeys.peerHistory(username ?? ''),
    queryFn: () => apiGet<PeerHistory>(`/api/peers/${encodeURIComponent(username ?? '')}`),
    enabled: username !== null,
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
//
// The poll stops while the stream carries this job's detail (issue #274).
// #161's design makes REST the source of truth for snapshots and polling the
// fallback "if the stream dies", but until now only the read half honoured
// that: on /jobs/:id the stream delivered a detail every second while this
// query fetched the identical object — same server-side toJobDetailDTO, so a
// whole response, not a partial view — every three seconds and pickJobDetail
// threw it away.
//
// Switching the interval off does not strand the cache key. StreamProvider
// invalidates it on every `onopen`, and invalidateQueries refetches an active
// query regardless of its interval, so each connection (including the
// browser's own automatic reconnect) still opens with a fresh GET — the
// "chain always starts with a GET" principle that this poll was masking
// rather than implementing. And REST remains the key's only writer:
// pickJobDetail chooses at read time and never writes the stream's object
// into the cache (issue #258).
export function useJobDetail(id: number) {
  const live = useLiveData();
  const detailQuery = useQuery({
    queryKey: queryKeys.jobDetail(id),
    queryFn: () => apiGet<WireJobDetail>(`/api/jobs/${id}/detail`).then(normalizeJobDetail),
    refetchInterval: streamCarriesJobDetail(live, id) ? false : JOB_DETAIL_INTERVAL,
  });
  return { ...detailQuery, data: pickJobDetail(detailQuery.data, live, id) };
}

// Unconditional, unlike useJobDetail above despite sharing its interval: the
// stream carries no job events at all (livePayload has no such field), so
// polling is the only source here on every route.
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

/**
 * Finished uploads, newest-first, one page at a time (issue #326).
 *
 * Modelled on useThread and carrying the same two traps. initialPageParam 0
 * means "no before= cursor" (beforeID 0 server-side); every later page's param
 * is the previous page's OLDEST row id, i.e. its LAST element, since the API
 * serves each page newest-first. getNextPageParam returns undefined once the
 * server says hasMore is false, or once a page comes back empty despite
 * claiming hasMore — without that second condition the button stays armed and
 * every press re-requests the same empty page forever.
 *
 * Deliberately no refetchInterval, unlike useUploads' 3s: history changes only
 * when a transfer ends, a finished upload is not urgent, and polling would
 * re-fetch every page the user has loaded on every tick. Fresh rows arrive
 * when the view remounts or when the user pages further back.
 */
export function useUploadHistory() {
  return useInfiniteQuery({
    queryKey: queryKeys.uploadHistory,
    queryFn: ({ pageParam }) => apiGet<UploadHistoryPage>(`/api/uploads/history?before=${pageParam}`),
    initialPageParam: 0,
    getNextPageParam: (lastPage) =>
      lastPage.hasMore && lastPage.uploads.length > 0
        ? lastPage.uploads[lastPage.uploads.length - 1].id
        : undefined,
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

/**
 * The scope a bulk retry acts on (issue #378): the status/source/search axes
 * of the view the user is looking at, and deliberately nothing else. `page`,
 * `sort` and `dir` are omitted rather than passed through — the endpoint
 * accepts and ignores them, and sending them would suggest the operation
 * respects the page on screen, which is the one thing it does not do.
 */
export function bulkRetryUrl(params: JobPageParams): string {
  const query = new URLSearchParams();
  query.set('filter', params.filter);
  query.set('source', params.source);
  query.set('q', params.q);
  return `/api/jobs/retry?${query.toString()}`;
}

/**
 * Revives every retryable job in the filtered set, not just the visible page.
 * Resolves with the server's own counts — a partial outcome is normal, so the
 * caller must report `retried`/`skipped` rather than treat the call as a
 * binary success.
 */
export function useBulkRetryJobs() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (params: JobPageParams) => apiPostJson<BulkRetryResult>(bulkRetryUrl(params)),
    onSuccess: () => {
      // useRetryJob names the one job's detail/events keys explicitly because
      // it knows which job moved; the response here does not say. It needs no
      // equivalent: ['jobs'] is a prefix of ['jobs', id, 'detail'] and
      // ['jobs', id, 'events'], so this one invalidation already covers every
      // cached job's detail and history.
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
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

// Reads a manual search session (issue #58): id is undefined before a search
// has been started (Search.tsx's idle state), which the query stays disabled
// for rather than firing a request against a nonsense URL.
//
// refetchInterval is a fallback, not the primary freshness mechanism — see
// SEARCH_POLL_INTERVAL's doc comment. While the session is still open
// (!done) on a genuinely streaming backend with a live connection, incoming
// `event: search` frames (folded in by api/stream.tsx via
// replaceSearchGroups, straight onto this same cache key) are what actually
// keep it current; this poll only takes over once that stops being true.
export function useSearchSession(id: string | undefined) {
  const live = useLiveData();
  const qc = useQueryClient();
  const key = id ?? '';
  return useQuery({
    queryKey: queryKeys.search(key),
    // Merges onto whatever the stream has already folded in rather than
    // replacing it — see mergeSearchSession for why a plain replace can drop
    // groups permanently.
    queryFn: () =>
      apiGet<WireSearchSession>(`/api/search/${key}`)
        .then(normalizeSearchSession)
        .then((fetched) => mergeSearchSession(qc.getQueryData<SearchSession>(queryKeys.search(key)), fetched)),
    enabled: id !== undefined,
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data || data.done) return false;
      // A batching backend (slskd) never sends incremental frames at all, so
      // the poll is the only freshness mechanism there — not a fallback.
      if (!data.streaming) return SEARCH_POLL_INTERVAL;
      // The shared EventSource errored (stream.tsx's clearLive writes null,
      // never undefined — see queryKeys.live). Arm immediately rather than
      // waiting out the staleness window below.
      //
      // `null` and `undefined` are NOT interchangeable here and this predicate
      // must never conflate them: `undefined` is the ordinary state on /search
      // with nothing downloading, because `event: live` only fires when a job
      // is actually live. An `live !== null` guard therefore reads as "the
      // stream is healthy" forever and disarms the poll permanently.
      if (live === null) return SEARCH_POLL_INTERVAL;
      // Otherwise arm on silence, which covers `live === undefined` and, more
      // importantly, the open-but-silent connection no connection-level check
      // can see at all. See SEARCH_STREAM_STALE_MS.
      //
      // While frames are still fresh this returns the time REMAINING until
      // they go stale rather than `false`. That distinction is load-bearing:
      // `false` schedules no timer at all, and React Query only re-evaluates
      // this predicate when the query's result changes — so a session whose
      // frames simply STOP would never be reconsidered, and the staleness
      // fallback could never fire for the one failure it exists to catch.
      // Returning the remaining time schedules the refetch exactly at the
      // staleness boundary instead; each frame that does arrive updates the
      // data, which recomputes this and pushes that boundary out again.
      const streamedAt = data.streamedAt;
      if (streamedAt !== undefined) {
        const remaining = streamedAt + SEARCH_STREAM_STALE_MS - Date.now();
        if (remaining > 0) return remaining;
      }
      return SEARCH_POLL_INTERVAL;
    },
  });
}

// Starts a new manual search session. Seeds queryKeys.search(id) with the
// 201 body directly on success — byte-identical in shape to what GET
// /api/search/{id} would return (see WireSearchSession's doc comment) — so
// the caller can render the first frame of results without a second round
// trip, and so useSearchStream/useSearchSession have something to patch onto
// the instant the id is published.
export function useStartSearch() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (query: string) => apiPostJson<WireSearchSession>('/api/search', { query }).then(normalizeSearchSession),
    onSuccess: (session) => {
      qc.setQueryData(queryKeys.search(session.id), session);
    },
  });
}

// Cancels a search session server-side (releasing its reserved Soulseek
// token early — see internal/app/search.go's Stop). Removes the cache entry
// outright rather than invalidating it: once stopped, GET /api/search/{id}
// answers 404, so there is nothing left to refetch.
export function useStopSearch() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiDelete(`/api/search/${id}`),
    onSuccess: (_void, id) => {
      qc.removeQueries({ queryKey: queryKeys.search(id) });
    },
  });
}

// The first frontend consumer of POST /api/jobs (issue #58's "Download
// album"/"Download selected" actions; #155 shipped the endpoint itself with
// none). Invalidates queryKeys.jobs so the next visit to /jobs shows the new
// job — the search view itself doesn't render a job list, so there's nothing
// more specific to patch here (see Search.tsx's local "queued" card state
// for the immediate feedback instead).
export function useCreateJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateJobRequest) => apiPostJson<WireJob>('/api/jobs', body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
    },
  });
}

// ---- MusicBrainz identify (issue #321) ----
//
// All three are plain GETs fired explicitly by IdentifyModal's own state
// machine (search button, album pick, edition/Lidarr lookups once an album
// is selected) rather than mounted queries — a `useQuery` with an `enabled`
// flag would refetch on every remount/window-focus for a resource the brief
// explicitly forbids polling or debouncing (the backend's identify cache has
// no eviction, sized for explicit-only traffic). `useMutation` gives the
// same isPending/isError bookkeeping the modal needs without any of that.

// `album` is required (the endpoint 422s on a blank one); `artist` is
// optional and, when present, narrows the combined MusicBrainz query rather
// than resolving to a separate artist id first — see IdentifyModal's
// searchMB for why this replaced an earlier two-call design.
export function useIdentifySearch() {
  return useMutation({
    mutationFn: ({ artist, album }: { artist?: string; album: string }) => {
      const params = new URLSearchParams();
      if (artist) params.set('artist', artist);
      params.set('album', album);
      return apiGet<MusicBrainzSearchResponse>(`/api/identify/search?${params.toString()}`);
    },
  });
}

export function useIdentifyEditions() {
  return useMutation({
    mutationFn: (albumId: string) =>
      apiGet<MusicBrainzEditionListResult>(`/api/identify/albums/${encodeURIComponent(albumId)}/editions`),
  });
}

export function useIdentifyLidarr() {
  return useMutation({
    mutationFn: (albumId: string) =>
      apiGet<LidarrMatch>(`/api/identify/albums/${encodeURIComponent(albumId)}/lidarr`),
  });
}

// ---- Lidarr add-artist flow (issue #331) ----
//
// Same explicit-mutation shape as the three identify hooks above, and for
// the same reason: fired by IdentifyModal's own state machine (artist pick,
// "add to Lidarr" opened, form submitted), never a mounted/polling query.

/** GET /api/lidarr/artists/{mbid} — the artist-level counterpart to useIdentifyLidarr. */
export function useLidarrArtistStatus() {
  return useMutation({
    mutationFn: (artistMbid: string) =>
      apiGet<LidarrArtistMatch>(`/api/lidarr/artists/${encodeURIComponent(artistMbid)}`),
  });
}

/** GET /api/lidarr/add-options — root folders and profiles for the "add to Lidarr" form, fetched once that path is opened. */
export function useLidarrAddOptions() {
  return useMutation({
    mutationFn: () => apiGet<LidarrAddOptions>('/api/lidarr/add-options'),
  });
}

/** POST /api/lidarr/artists — ensures the artist exists in the library, unmonitored. */
export function useAddLidarrArtist() {
  return useMutation({
    mutationFn: (body: AddLidarrArtistRequest) => apiPostJson<AddLidarrArtistResult>('/api/lidarr/artists', body),
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
