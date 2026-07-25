import {
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { apiGet, apiPost, apiPostJson } from './client';
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
  ShareRescanResult,
  SharesReport,
  StatusReport,
} from './types';

// Intervals match the legacy dashboard exactly so perceived freshness is
// unchanged by the migration.
const JOBS_INTERVAL = 3000;
const EVENTS_INTERVAL = 3000;
const STATUS_INTERVAL = 5000;
const PEERS_INTERVAL = 5000;
const CHARTS_INTERVAL = 15000; // passes change at most every discovery tick (~30s)
const SHARES_INTERVAL = 15000;
const SHARES_SCANNING_INTERVAL = 3000; // matches JOBS_INTERVAL while a scan is actively running

export const queryKeys = {
  jobs: ['jobs'] as const,
  status: ['status'] as const,
  events: ['events'] as const,
  peers: ['peers'] as const,
  config: ['config'] as const,
  charts: ['charts'] as const,
  shares: ['shares'] as const,
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
