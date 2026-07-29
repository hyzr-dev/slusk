// api/stream.tsx: the frontend half of issue #161's SSE live stream. See
// internal/observ/stream.go for the server contract this consumes and
// queries.ts's replaceLiveJobs/pickJobDetail/mergeThroughputSamples for how
// the frame this writes is folded into what components read.
import { createContext, useContext, useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useLocation } from 'react-router-dom';
import { mergeThroughputSamples, queryKeys } from './queries';
import type { ChartsReport, LivePayload, ScopedLivePayload, WireJob } from './types';

// Recognises a job detail route (/jobs/:id) from the current pathname.
// StreamProvider is mounted above <Routes> (see App.tsx), not as a route
// element itself, so `useParams` isn't available here — pattern-matching the
// pathname is the only way to know which job, if any, is on screen.
const JOB_ROUTE = /^\/jobs\/(\d+)(?:\/|$)/;

function jobIdFromPathname(pathname: string): number | undefined {
  const match = JOB_ROUTE.exec(pathname);
  return match ? Number(match[1]) : undefined;
}

// Caps the per-connection job accumulator (see jobsById in StreamProvider).
// Well above any plausible live set — the largest realistic count of
// simultaneously live-matched jobs is in the low hundreds (max_active
// defaults to 30; a lab sync's ~150-job seed is the largest count seen in
// practice) — so ordinary use never reaches it. An unscoped connection
// (any route that neither is /jobs/:id nor calls useJobScope — e.g.
// Settings, Peers, Chat) would otherwise accumulate every job that has ever
// gone live for the connection's whole lifetime, since the backend simply
// stops streaming a job once it leaves the live-matched set and nothing
// else ever removes its entry. Evicting past this cap is safe by
// construction: falling back to the REST row is
// always correct (see replaceLiveJobs in queries.ts) — that is exactly what
// happened before accumulation existed — so dropping the oldest entry can
// only make a value briefly stale, never wrong.
export const JOBS_CACHE_LIMIT = 500;

// Lets a route publish the job ids currently on screen (the /jobs list's
// current page) so StreamProvider can open the connection scoped to them —
// `?jobs=1,2,3` — instead of receiving every live-matched job in the app.
// Default no-op setter covers rendering outside StreamProvider (tests that
// mount a route in isolation).
const JobScopeSetterContext = createContext<(ids: number[] | undefined) => void>(() => {});

/**
 * Publishes `ids` as the current job scope for the SSE connection (issue
 * #258). Call from a route that owns a bounded page of ids — Jobs.tsx and
 * Overview.tsx both do (issue #268). JobDetail needs no call here at all:
 * its scoping comes from the `?job=<id>` branch below, derived straight
 * from the route (jobIdFromPathname), which is a single id rather than a
 * page of them.
 *
 * Dedupes on the ids joined into a string rather than on `ids` itself: the
 * array is rebuilt fresh on every render and on every 15s REST poll, so
 * comparing by reference would reopen the EventSource in a loop even when
 * the page's ids haven't actually changed.
 */
export function useJobScope(ids: number[] | undefined): void {
  const setScope = useContext(JobScopeSetterContext);
  const key = ids?.join(',');
  useEffect(() => {
    setScope(ids);
    return () => setScope(undefined);
    // `key` stands in for `ids`: same joined string means the same scope, and
    // `ids` at the time this effect runs is always the array that produced it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
}

/**
 * One EventSource for the whole app, opened at `/api/stream` or
 * `/api/stream?job=<id>` on a job detail route — one connection per view,
 * not per panel, so JobDetail's per-file data and the header's aggregate
 * `down` ride the same connection. Reopens whenever the job-scoped route
 * changes.
 *
 * Every `event: live` frame is written into queryKeys.live via
 * setQueryData, and cleared to null on a genuine stream failure so every
 * consumer (useJobs, useJobDetail, TopBar) reverts to plain REST values — see
 * queries.ts's merge functions for the read side. There is deliberately no
 * custom reconnect logic: `EventSource` reconnects on its own and the
 * server sets `retry:` (internal/observ/stream.go); `onopen` instead
 * invalidates the REST queries so every reconnect — including the browser's
 * automatic one — takes a fresh snapshot, exactly like a page load. This
 * mirrors the #161 design doc's chosen reconnect strategy: no
 * `Last-Event-ID`, no replay.
 *
 * `jobs` is a per-frame *delta* of changed jobs, not a snapshot of the live
 * set (see LivePayload's doc comment) — so frames are accumulated by id in
 * `jobsById` below, newest entry per id winning, rather than each frame
 * replacing the cache outright. Without this, a job that stops changing
 * while others keep ticking would fall out of the very next frame and
 * appear to revert to its last REST value until the next 15s poll (issue
 * #258's fix-round bug: a finished download would flash back to
 * downloading). `jobsById` itself is always fresh per connection (reset at
 * the top of this effect), but the *cache* (queryKeys.live) is deliberately
 * NOT cleared just because this effect reruns to open a new connection —
 * see the effect body for why (issue #276).
 *
 * Independently of the job-detail scope above, a route with a bounded page
 * of jobs on screen can narrow `jobs` in the `live` frame to that page via
 * useJobScope — see JobScopeSetterContext. The two scopes are unrelated:
 * `?job=<id>` on /jobs/:id always wins when present, since a detail page's
 * own `detail` field needs the connection scoped to that one job regardless
 * of which jobs happen to be on the /jobs list underneath it.
 */
export function StreamProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const { pathname } = useLocation();
  const jobId = jobIdFromPathname(pathname);
  const [jobsScope, setJobsScope] = useState<number[] | undefined>(undefined);
  // Joined into the effect's dependency array (see useJobScope) rather than
  // `jobsScope` itself, so a same-content page of ids published again
  // doesn't reopen the connection.
  const jobsScopeKey = jobsScope?.join(',');

  // Known cost, tracked as #267, not fixed here: mounting a scope-publishing
  // route (/jobs) opens two connections in quick succession — an unscoped
  // one on this component's first commit (jobsScope is still its initial
  // `undefined`), then the real `?jobs=...` one once useJobScope's effect
  // has run in the child and published into jobsScope, changing
  // jobsScopeKey and reopening below. Each open runs onopen's invalidation
  // and the server's refreshCorrelation. Avoiding the first open would need
  // the scope to be known synchronously during this component's first
  // render (e.g. a ref-driven subscription written during render rather
  // than an effect), which is a real restructuring of how useJobScope
  // publishes — left alone for now since it only costs one extra connection
  // at mount, not on every poll.

  useEffect(() => {
    const url = jobId !== undefined
      ? `/api/stream?job=${jobId}`
      : jobsScope !== undefined
        ? `/api/stream?jobs=${jobsScope.join(',')}`
        : '/api/stream';
    const source = new EventSource(url);

    // Accumulates each frame's changed jobs by id — see the doc comment
    // above and JOBS_CACHE_LIMIT for the eviction bound.
    let jobsById = new Map<number, WireJob>();

    // null, not undefined: queryClient.setQueryData ignores an undefined
    // value outright (query-core's `if (data === void 0) return void 0`), so
    // clearing with undefined is a silent no-op that would leave the last
    // live frame overlaid forever after the stream dies — the exact failure
    // the REST fallback exists to prevent. null is a real cached value, so
    // it both persists and notifies subscribers; the merge functions treat
    // null and undefined identically ("nothing live right now").
    //
    // jobsById is reassigned to a fresh Map here rather than cleared in
    // place — the two are equivalent (the map is closure-local and never
    // escapes except via the Array.from(...) copy below), this is just the
    // simpler statement to reset it alongside the query cache.
    const clearLive = () => {
      jobsById = new Map();
      queryClient.setQueryData(queryKeys.live, null);
    };

    source.onopen = () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs });
      void queryClient.invalidateQueries({ queryKey: queryKeys.charts });
      if (jobId !== undefined) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.jobDetail(jobId) });
      }
    };

    source.addEventListener('live', (event) => {
      const payload = JSON.parse((event as MessageEvent<string>).data) as LivePayload;
      for (const job of payload.jobs ?? []) {
        // delete-then-set moves a re-updated job to the end of the Map's
        // insertion order, so the eviction below always drops the entry
        // that has gone longest without changing — an actively-updating job
        // is never the one evicted.
        jobsById.delete(job.id);
        jobsById.set(job.id, job);
      }
      while (jobsById.size > JOBS_CACHE_LIMIT) {
        const oldest = jobsById.keys().next().value;
        if (oldest === undefined) break;
        jobsById.delete(oldest);
      }
      const scoped: ScopedLivePayload = { ...payload, jobs: Array.from(jobsById.values()), scopeJobId: jobId };
      queryClient.setQueryData(queryKeys.live, scoped);
      if (payload.throughput?.length || payload.uploadThroughput?.length) {
        // Only folds in once a REST snapshot already exists — if none has
        // landed yet there's nothing to append to, and the imminent REST
        // fetch will carry these samples anyway. Each direction merges into
        // its own 48-sample window, even when a frame updates both at once.
        queryClient.setQueryData<ChartsReport>(queryKeys.charts, (prev) => {
          if (!prev) return prev;
          return {
            ...prev,
            throughput: payload.throughput?.length
              ? mergeThroughputSamples(prev.throughput, payload.throughput)
              : prev.throughput,
            uploadThroughput: payload.uploadThroughput?.length
              ? mergeThroughputSamples(prev.uploadThroughput, payload.uploadThroughput)
              : prev.uploadThroughput,
          };
        });
      }
    });

    // EventSource's error event fires both for a genuine failure and for
    // the moment just before its own automatic reconnect — either way there
    // is currently no live stream, so clearing here (rather than only in the
    // cleanup below) is what makes a mid-session drop revert consumers to
    // REST immediately instead of only at the next route change.
    source.onerror = clearLive;

    // Deliberately does NOT call clearLive: this effect reruns whenever
    // jobId/jobsScopeKey changes, which is the common case (Overview and
    // Jobs republish their scope on every poll as job state churns — see
    // useJobScope), not just on a genuine unmount. The accumulated jobsById
    // entries are still valid values for jobs that stay in scope, and the
    // new connection's initial snapshot (streamHub.subscribe on the Go side)
    // resends every job in the new scope regardless — so wiping the cache
    // here only produced a visible blank frame for no benefit (issue #276:
    // every /api/jobs poll that shifted the Overview panel's active+stalled
    // membership blanked every row on screen). The one path that must still
    // clear on close is a real stream failure, handled above by onerror; a
    // genuine unmount of StreamProvider itself is handled by the separate
    // effect below.
    return () => {
      source.close();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- jobsScopeKey stands in for jobsScope, see above
  }, [queryClient, jobId, jobsScopeKey]);

  // Clears the live cache only when StreamProvider itself unmounts — as
  // opposed to the effect above, which reruns (closing and reopening the
  // connection) on every scope change without touching the cache. Empty-ish
  // deps ([queryClient], stable for the app's lifetime) means this cleanup
  // fires exactly once, on real unmount, not on a reconnect. StreamProvider
  // is mounted above <Routes> (see its own doc comment), so in practice this
  // only fires if the whole provider tree is torn down — but leaving a stale
  // live overlay cached indefinitely in that case is exactly the failure the
  // REST fallback exists to prevent.
  useEffect(() => {
    return () => {
      queryClient.setQueryData(queryKeys.live, null);
    };
  }, [queryClient]);

  return (
    <JobScopeSetterContext.Provider value={setJobsScope}>
      {children}
    </JobScopeSetterContext.Provider>
  );
}
