// api/stream.ts: the frontend half of issue #161's SSE live stream. See
// internal/observ/stream.go for the server contract this consumes and
// queries.ts's mergeLiveJobs/pickJobDetail/mergeThroughputSamples for how
// the frame this writes is folded into what components read.
import { useEffect } from 'react';
import type { ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useLocation } from 'react-router-dom';
import { mergeThroughputSamples, queryKeys } from './queries';
import type { ChartsReport, LivePayload, ScopedLivePayload } from './types';

// Recognises a job detail route (/jobs/:id) from the current pathname.
// StreamProvider is mounted above <Routes> (see App.tsx), not as a route
// element itself, so `useParams` isn't available here — pattern-matching the
// pathname is the only way to know which job, if any, is on screen.
const JOB_ROUTE = /^\/jobs\/(\d+)(?:\/|$)/;

function jobIdFromPathname(pathname: string): number | undefined {
  const match = JOB_ROUTE.exec(pathname);
  return match ? Number(match[1]) : undefined;
}

/**
 * One EventSource for the whole app, opened at `/api/stream` or
 * `/api/stream?job=<id>` on a job detail route — one connection per view,
 * not per panel, so JobDetail's per-file data and the header's aggregate
 * `down` ride the same connection. Reopens whenever the job-scoped route
 * changes.
 *
 * Every `event: live` frame is written into queryKeys.live via
 * setQueryData, and cleared to undefined on error/close so every consumer
 * (useJobs, useJobDetail, TopBar) reverts to plain REST values — see
 * queries.ts's merge functions for the read side. There is deliberately no
 * custom reconnect logic: `EventSource` reconnects on its own and the
 * server sets `retry:` (internal/observ/stream.go); `onopen` instead
 * invalidates the REST queries so every reconnect — including the browser's
 * automatic one — takes a fresh snapshot, exactly like a page load. This
 * mirrors the #161 design doc's chosen reconnect strategy: no
 * `Last-Event-ID`, no replay.
 */
export function StreamProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const { pathname } = useLocation();
  const jobId = jobIdFromPathname(pathname);

  useEffect(() => {
    const url = jobId === undefined ? '/api/stream' : `/api/stream?job=${jobId}`;
    const source = new EventSource(url);

    // null, not undefined: queryClient.setQueryData ignores an undefined
    // value outright (query-core's `if (data === void 0) return void 0`), so
    // clearing with undefined is a silent no-op that would leave the last
    // live frame overlaid forever after the stream dies — the exact failure
    // the REST fallback exists to prevent. null is a real cached value, so
    // it both persists and notifies subscribers; the merge functions treat
    // null and undefined identically ("nothing live right now").
    const clearLive = () => queryClient.setQueryData(queryKeys.live, null);

    source.onopen = () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs });
      void queryClient.invalidateQueries({ queryKey: queryKeys.charts });
      if (jobId !== undefined) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.jobDetail(jobId) });
      }
    };

    source.addEventListener('live', (event) => {
      const payload = JSON.parse((event as MessageEvent<string>).data) as LivePayload;
      const scoped: ScopedLivePayload = { ...payload, scopeJobId: jobId };
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

    return () => {
      source.close();
      clearLive();
    };
  }, [queryClient, jobId]);

  return children;
}
