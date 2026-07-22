// Hand-written mirrors of the Go DTOs in internal/observ. Kept in one file so
// drift has a single place to be caught. See spec 2026-07-20.

// dashboardStatus() in internal/observ/status.go only ever returns these five
// values for a job's own status; "orphaned" exists solely as an aggregate
// count on StatusReport, never as an individual job's status.
export type JobStatus = 'queued' | 'active' | 'stalled' | 'done' | 'failed';
export type JobState =
  | 'WANTED' | 'SELECTING' | 'DOWNLOADING' | 'IMPORTING'
  | 'DONE' | 'FAILED' | 'CANCELLED';
export type CandidateState = 'NEW' | 'ACTIVE' | 'SUCCEEDED' | 'FAILED';

/** GET /api/jobs — internal/observ/observ.go jobDTO */
export interface Job {
  id: number;
  title: string;
  artist: string;
  status: JobStatus;
  peer: string;
  bytesDone: number;
  bytesTotal: number;
  updatedAt: string;
  state: JobState;
  candidatesTried: number;
  maxCandidates: number;
  failReason: string;
  nextAttemptAt: string;
  retries: number;
  notBefore: string;
}

/** internal/observ/jobdetail.go transferDetailDTO */
export interface TransferDetail {
  filename: string;
  state: string;
  bytesDone: number;
  bytesTotal: number;
  retries: number;
  lastProgressAt: string;
  // Live, non-persisted values the native backend joins in from ListDownloads;
  // omitempty on the Go side means they are absent (not zero) for queued-only,
  // actively-downloading, or terminal transfers respectively — so treat absent
  // as "hide" rather than showing a misleading 0.
  queuePosition?: number;
  speed?: number;
}

/** internal/observ/jobdetail.go attemptDetailDTO */
export interface AttemptDetail {
  id: number;
  username: string;
  fileCount: number;
  state: CandidateState;
  failReason: string;
  createdAt: string;
  updatedAt: string;
  transfers: TransferDetail[];
}

/** GET /api/jobs/{id}/detail — jobDetailDTO */
export interface JobDetail {
  id: number;
  title: string;
  artist: string;
  state: JobState;
  attempts: AttemptDetail[];
}

/** GET /api/events and /api/jobs/{id}/events — eventDTO */
export interface JobEvent {
  id: number;
  jobId: number;
  event: string;
  detail: string;
  createdAt: string;
}

/** internal/observ/peers.go peerArtistDTO */
export interface PeerArtist {
  artistId: number;
  successCount: number;
  failCount: number;
  lastSuccessAt: string;
  lastFailAt: string;
  score: number;
}

/** GET /api/peers — peerDTO */
export interface Peer {
  username: string;
  successCount: number;
  failCount: number;
  lastSuccessAt: string;
  lastFailAt: string;
  score: number;
  artists: PeerArtist[];
}

/** internal/observ/observ.go moduleStatusDTO */
export interface ModuleStatus {
  lastAttempt: string;
  lastCompleted: string;
  lastSuccess: string;
  lastErrorAt: string;
  lastError: string;
  consecutiveFailures: number;
  staleDeadline: string;
  live: boolean;
  ready: boolean;
}

/** GET /status */
export interface StatusReport {
  queued: number;
  active: number;
  stalled: number;
  orphaned: number;
  modules: Record<string, string>;
  moduleDetails: Record<string, ModuleStatus>;
}

/**
 * GET /api/config — see Task 14. Secrets are never sent, only their
 * presence. Field names mirror the [pipeline] TOML keys (internal/observ/
 * config.go AppConfig) so the settings view can display something that
 * matches the config file the user actually edits.
 */
export interface AppConfig {
  lidarrUrl: string;
  lidarrApiKeyConfigured: boolean;
  wantedSyncInterval: string;
  maxActive: number;
  soulseekEnabled: boolean;
}

/**
 * POST /api/config/test/{lidarr,soulseek} — connectionTestResult. ok is true
 * when the dependency answered; error is a human-readable reason otherwise and
 * never contains secrets.
 */
export interface ConnectionTestResult {
  ok: boolean;
  error?: string;
}

/** internal/observ/charts.go passDTO — one completed Discovery search cycle. */
export interface SearchPass {
  startedAt: string;
  finishedAt: string;
  searched: number;
  matched: number;
}

/** internal/observ/charts.go hourCountDTO — one hour bucket of a count series. */
export interface HourCount {
  hour: string;
  count: number;
}

/**
 * GET /api/charts — chartsDTO. passes is oldest-first, capped at 20;
 * completedByHour is always exactly 24 zero-filled hourly buckets, oldest
 * first, ending at the current hour.
 */
export interface ChartsReport {
  passes: SearchPass[];
  completedByHour: HourCount[];
}
