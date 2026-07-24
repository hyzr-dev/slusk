// Hand-written mirrors of the Go DTOs in internal/observ. Kept in one file so
// drift has a single place to be caught. See spec 2026-07-20.

// dashboardStatus() in internal/observ/status.go returns these six values for
// a job's own status. "orphaned" is also aggregated as a count on
// StatusReport (issue #158).
export type JobStatus = 'queued' | 'active' | 'stalled' | 'done' | 'failed' | 'orphaned';
export type JobState =
  | 'WANTED' | 'SELECTING' | 'DOWNLOADING' | 'IMPORTING'
  | 'DONE' | 'FAILED' | 'CANCELLED' | 'ORPHANED';
export type CandidateState = 'NEW' | 'ACTIVE' | 'SUCCEEDED' | 'FAILED';

// JobSource distinguishes a Lidarr wanted-sync job from one created manually
// via POST /api/jobs (issue #155).
export type JobSource = 'lidarr' | 'manual';

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
  source: JobSource;
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

/** internal/observ/config.go LidarrView */
export interface LidarrConfigDTO {
  url: string;
  apiKeyConfigured: boolean;
}

/** internal/observ/config.go SlskdView */
export interface SlskdConfigDTO {
  url: string;
  apiKeyConfigured: boolean;
}

/** internal/observ/config.go WeightsView — [pipeline.weights] TOML keys. */
export interface PipelineWeightsDTO {
  format: number;
  bitrate: number;
  reliability: number;
  fileCount: number;
  knownUser: number;
}

/**
 * internal/observ/config.go PipelineView. Durations are Go's
 * time.Duration.String() form (e.g. "1h0m0s", "45s") — always parseable as
 * plain text, no client-side parsing needed.
 */
export interface PipelineConfigDTO {
  backend: 'slskd' | 'soulseek';
  maxCandidatesPerAlbum: number;
  maxActive: number;
  maxRetries: number;
  maxInflightPerPeer: number;
  maxTransferRetries: number;
  minBitrate: number;
  transferDeadline: string;
  stallTimeout: string;
  searchTimeout: string;
  backoffBase: string;
  backoffCap: string;
  candidateTtl: string;
  failedReviveAfter: string;
  stuckAfter: string;
  tickTimeout: string;
  importConfirmTimeout: string;
  wantedSyncInterval: string;
  discoveryInterval: string;
  selectingInterval: string;
  downloadingInterval: string;
  importingInterval: string;
  manualImportTimeout: string;
  importRetryCooldown: string;
  weights: PipelineWeightsDTO;
}

/** internal/observ/config.go SharedFolderView — one [[soulseek.shared_folders]] entry. */
export interface SharedFolderDTO {
  name: string;
  path: string;
}

/** internal/observ/config.go GluetunView — [soulseek.gluetun]. */
export interface GluetunConfigDTO {
  controlUrl: string;
  apiKeyConfigured: boolean;
}

/**
 * internal/observ/config.go SoulseekView. enabled is derived
 * server-side from whether the section is configured, and is never sent
 * back in the POST body.
 */
export interface SoulseekConfigDTO {
  enabled: boolean;
  serverAddress: string;
  username: string;
  passwordConfigured: boolean;
  listenAddr: string;
  uploadSlots: number;
  allowPrivatePeerAddresses: boolean;
  gluetun: GluetunConfigDTO;
  sharedFolders: SharedFolderDTO[];
}

/** internal/observ/config.go StoreView. */
export interface StoreConfigDTO {
  dsnConfigured: boolean;
}

/**
 * internal/observ/config.go ObservView. logLevel may be "" (meaning
 * "use the default, info") or one of debug/info/warn/error.
 */
export interface ObservConfigDTO {
  listenAddr: string;
  authTokenConfigured: boolean;
  logLevel: string;
}

/** internal/observ/config.go PathsView. */
export interface PathsConfigDTO {
  slskdCompleteDir: string;
}

/**
 * GET /api/config — internal/observ/config.go AppConfig. Every non-secret
 * field is always present, nested by TOML section; secrets are never sent,
 * only their presence via the *Configured booleans. writable reports
 * whether the config file's directory currently accepts writes (see
 * config.ProbeWritable) — false renders a read-only view instead of the
 * editable form.
 */
export interface AppConfig {
  lidarr: LidarrConfigDTO;
  slskd: SlskdConfigDTO;
  pipeline: PipelineConfigDTO;
  soulseek: SoulseekConfigDTO;
  store: StoreConfigDTO;
  observ: ObservConfigDTO;
  paths: PathsConfigDTO;
  writable: boolean;
}

/**
 * POST /api/config request body — internal/observ/config.go
 * configUpdateRequest. All non-secret fields are always included (there is
 * no partial-update semantics: the form always submits the full current
 * state of every field). Each secret field below is omitted (or blank) to
 * mean "keep the currently configured value"; the settings view never
 * receives a secret back, so it has no way to resend one unchanged.
 */
export interface ConfigUpdateRequest {
  lidarr: { url: string; apiKey?: string };
  slskd: { url: string; apiKey?: string };
  pipeline: {
    backend: 'slskd' | 'soulseek';
    maxCandidatesPerAlbum: number;
    maxActive: number;
    maxRetries: number;
    maxInflightPerPeer: number;
    maxTransferRetries: number;
    minBitrate: number;
    transferDeadline: string;
    stallTimeout: string;
    searchTimeout: string;
    backoffBase: string;
    backoffCap: string;
    candidateTtl: string;
    failedReviveAfter: string;
    stuckAfter: string;
    tickTimeout: string;
    importConfirmTimeout: string;
    wantedSyncInterval: string;
    discoveryInterval: string;
    selectingInterval: string;
    downloadingInterval: string;
    importingInterval: string;
    manualImportTimeout: string;
    importRetryCooldown: string;
    weights: PipelineWeightsDTO;
  };
  // soulseek.enabled is derived server-side and deliberately absent here.
  soulseek: {
    serverAddress: string;
    username: string;
    password?: string;
    listenAddr: string;
    uploadSlots: number;
    allowPrivatePeerAddresses: boolean;
    gluetun: { controlUrl: string; apiKey?: string };
    sharedFolders: SharedFolderDTO[];
  };
  store: { dsn?: string };
  observ: { listenAddr: string; authToken?: string; logLevel: string };
  paths: { slskdCompleteDir: string };
}

/** POST /api/config 200 response body. */
export interface ConfigUpdateResult {
  ok: boolean;
  restarting: boolean;
}

/**
 * Error body shared by POST /api/config's 400/409/422/500 responses.
 * fieldErrors is present only for a 422 validation failure, keyed by a
 * dot-path into the request body matching its own (camelCase) key names,
 * e.g. "pipeline.maxActive" or "soulseek.sharedFolders[0].name". A 422 can
 * also carry an empty fieldErrors object when the failure is a cross-field
 * rule with no single field to attach to — the message in `error` is the
 * thing to show in that case.
 */
export interface ApiErrorBody {
  error: string;
  fieldErrors?: Record<string, string>;
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
