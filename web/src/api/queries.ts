import {
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { apiDelete, apiGet, apiPost, apiPostJson } from './client';
import type {
  AppConfig,
  ChartsReport,
  ConfigUpdateRequest,
  ConfigUpdateResult,
  ConnectionTestResult,
  Job,
  JobDetail,
  JobEvent,
  Peer,
  StatusReport,
} from './types';

// Intervals match the legacy dashboard exactly so perceived freshness is
// unchanged by the migration.
const JOBS_INTERVAL = 3000;
const EVENTS_INTERVAL = 3000;
const STATUS_INTERVAL = 5000;
const PEERS_INTERVAL = 5000;
const CHARTS_INTERVAL = 15000; // passes change at most every discovery tick (~30s)

export const queryKeys = {
  jobs: ['jobs'] as const,
  status: ['status'] as const,
  events: ['events'] as const,
  peers: ['peers'] as const,
  config: ['config'] as const,
  charts: ['charts'] as const,
  jobDetail: (id: number) => ['jobs', id, 'detail'] as const,
  jobEvents: (id: number) => ['jobs', id, 'events'] as const,
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
// `enabled` defaults to true (JobDetail's own usage); the Jobs list expansion
// panel passes it explicitly so the detail fetch only starts once a row is
// expanded, rather than for every row up front.
export function useJobDetail(id: number, options: { enabled?: boolean } = {}) {
  return useQuery({
    queryKey: queryKeys.jobDetail(id),
    queryFn: () => apiGet<JobDetail>(`/api/jobs/${id}/detail`),
    refetchInterval: JOBS_INTERVAL,
    enabled: options.enabled ?? true,
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
