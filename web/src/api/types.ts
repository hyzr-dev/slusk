// Hand-written mirrors of the Go DTOs in internal/observ. Kept in one file so
// drift has a single place to be caught. See spec 2026-07-20.

// Canonical UI values. Legacy wire values are kept separate below and
// normalized before React Query caches a response. 'importing' is a real
// per-job status now (issue #269) — the backend used to serialize an
// IMPORTING job's status as 'active' (Tag derived the IM tag separately from
// `state`), a drift between the SQL and Go copies of this rule that this
// value removes the need for.
export type JobStatus = 'queued' | 'active' | 'stalled' | 'importing' | 'done' | 'failed' | 'parked';
export type JobState =
  | 'WANTED' | 'SELECTING' | 'DOWNLOADING' | 'IMPORTING'
  | 'DONE' | 'FAILED' | 'CANCELLED' | 'PARKED';
export type WireJobStatus = JobStatus | 'orphaned';
export type WireJobState = JobState | 'ORPHANED';
export type CandidateState = 'NEW' | 'ACTIVE' | 'SUCCEEDED' | 'FAILED';

// JobSource distinguishes a Lidarr wanted-sync job from one created manually
// via POST /api/jobs (issue #155).
export type JobSource = 'lidarr' | 'manual';

/**
 * GET /api/jobs query primitives. The backend defaults each page to
 * `jobsPageSize` (12) unless `pageSize` is given — see JobPageParams.
 * 'transfer' exists for Overview's TRANSFERS panel (issue #268):
 * `transferOrder` moved server-side (status group first — active before
 * stalled — then createdAt ascending within the group, see the now-deleted
 * client copy of this rule that used to live in web/src/routes/jobSort.ts).
 * 'inflight' and 'finished' are Overview's two region filters (issue #287):
 * 'inflight' is every job the pipeline holds a MaxActive slot for, 'finished'
 * is a job that reached a terminal state within the backend's recent window.
 */
export type JobPageSort = 'st' | 'album' | 'peer' | 'try' | 'transfer' | 'recent';
export type JobPageDirection = 'asc' | 'desc';
export type JobStatusFilter =
  | 'all'
  | 'active'
  | 'importing'
  | 'queued'
  | 'stalled'
  | 'failed'
  | 'parked'
  | 'done'
  | 'inflight'
  | 'finished';
export type JobSourceFilter = 'all' | JobSource;

export interface JobPageParams {
  page: number;
  sort: JobPageSort;
  dir: JobPageDirection;
  filter: JobStatusFilter;
  source: JobSourceFilter;
  q: string;
  /**
   * Rows per page, 1-50. Omitted means the backend's default (`jobsPageSize`,
   * 12) — the paged Jobs route relies on that default rather than sending it
   * explicitly. Overview (issue #268) requests 8.
   */
  pageSize?: number;
  /**
   * Opt out of `total` and the facet counts (`facets=0`). The server's facet
   * query is the expensive part of `/api/jobs` and runs whatever the filter is,
   * so a panel that renders neither should not ask for them. Leave unset in any
   * view that reads `total` or renders facet chips.
   */
  skipFacets?: boolean;
}

export interface JobStatusFacets {
  all: number;
  active: number;
  importing: number;
  queued: number;
  stalled: number;
  failed: number;
  parked: number;
  done: number;
}

export interface JobSourceFacets {
  all: number;
  manual: number;
  lidarr: number;
}

export interface JobFacets {
  status: JobStatusFacets;
  source: JobSourceFacets;
}

/** GET /api/jobs — internal/observ/observ.go jobDTO */
export interface Job {
  id: number;
  title: string;
  artist: string;
  status: JobStatus;
  peer: string;
  // Album totals summed across every file of the job's current candidate,
  // not just the most recently updated transfer (issue #174) — so the
  // progress bar doesn't jump backwards when a new file starts.
  bytesDone: number;
  bytesTotal: number;
  // When the job was first created — unlike updatedAt this never changes on
  // progress/state updates, so it's used to sort the Overview TRANSFERS panel
  // by start order (#233) without rows reordering on every progress tick.
  createdAt: string;
  updatedAt: string;
  /** When this DTO instance was computed server-side — see internal/observ's
   * jobDTO.FramedAt. Used (only for stream-sourced jobs) to decide whether
   * live data is still fresh enough to trust over REST — see replaceLiveJobs. */
  framedAt: string;
  state: JobState;
  candidatesTried: number;
  maxCandidates: number;
  failReason: string;
  nextAttemptAt: string;
  retries: number;
  notBefore: string;
  source: JobSource;
  year: number | null;
  tracks: number | null;
  format: string | null;
  // Live, non-persisted values aggregated across every live transfer
  // belonging to the job's current candidate (see aggregateLiveAlbum, issue
  // #157) — album-level analogues of TransferDetail's own queuePosition/speed.
  // omitempty on the Go side means they are absent (not zero) for a job with
  // no candidate yet, or none currently in flight — so treat absent as "hide"
  // rather than showing a misleading 0. etaSeconds is a duration to format,
  // not a timestamp.
  queuePosition?: number;
  speed?: number;
  etaSeconds?: number;
}

/** Job wire shape during the orphaned-to-parked transition. */
export type WireJob = Omit<Job, 'status' | 'state'> & {
  status: WireJobStatus;
  state: WireJobState;
};

/** GET /api/jobs response after normalization. */
export interface JobPage {
  jobs: Job[];
  total: number;
  facets: JobFacets;
}

/** GET /api/jobs response before nested jobs are normalized. */
export interface WireJobPage extends Omit<JobPage, 'jobs'> {
  jobs: WireJob[];
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

/**
 * GET /api/jobs/{id}/detail — jobDetailDTO. `job` is a whole jobDTO (issue
 * #268), built by the same toJobDTO the REST job list and the live stream
 * already use — not a hand-picked subset of fields chosen for whatever
 * JobDetail.tsx happened to render at the time, which is what the previous
 * flat `id/title/artist/state` shape was drifting toward. This is also what
 * lets the detail header stay live for free: the stream's `?job=<id>` frame
 * already carries the whole detail body, `job` included, so it updates in
 * the same frame as everything else on the page (see pickJobDetail in
 * queries.ts).
 */
export interface JobDetail {
  job: Job;
  attempts: AttemptDetail[];
}

/** GET /api/jobs/{id}/detail wire shape during the state-name transition. */
export type WireJobDetail = Omit<JobDetail, 'job'> & { job: WireJob };

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
  parked: number;
  modules: Record<string, string>;
  moduleDetails: Record<string, ModuleStatus>;
  /**
   * The running build: a `v*` tag in a deployed container, `dev` for a binary
   * built without the ldflag. Optional because an older server predating
   * issue #229 omits it entirely, and the top bar then shows nothing rather
   * than an empty slot.
   */
  version?: string;
}

/** GET /status wire shape; old servers omit parked, new servers may omit orphaned. */
export type WireStatusReport = Omit<StatusReport, 'parked'> & {
  parked?: number;
  orphaned?: number;
};

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

/** internal/observ/shares.go ShareFolderStats — one configured share folder's contribution to the index, one entry of SharesReport.folders. */
export interface ShareFolder {
  name: string;
  path: string;
  directories: number;
  files: number;
  totalBytes: number;
}

/**
 * GET /api/shares — internal/observ/shares.go sharesDTO. The endpoint always
 * answers 200; enabled (not a failed request) is what distinguishes "native
 * Soulseek sharing is off in the configuration" from "sharing is on but no
 * folders are configured yet" — folders is [] and every stat is 0 in both
 * cases, so enabled is the only reliable signal. indexedAt is "" when no scan
 * has ever completed.
 */
export interface SharesReport {
  enabled: boolean;
  scanning: boolean;
  indexedAt: string;
  scanDurationMs: number;
  directories: number;
  files: number;
  totalBytes: number;
  folders: ShareFolder[];
}

/** POST /api/shares/rescan 202 response body. */
export interface ShareRescanResult {
  ok: boolean;
  scanning: boolean;
}

/**
 * internal/observ/uploads.go UploadEntry — one upload the native Soulseek
 * upload manager knows about, either currently streaming or waiting in the
 * queue. Filename is the normalized virtual share path (backslash-separated),
 * not a host filesystem path. `active` true means `position` is 0
 * (meaningless for an active upload); a `position` of 0 with `active` false
 * never occurs. `size` is 0 until the transfer's share entry resolves, so
 * render it as unknown rather than a 0-byte file. `bytesWritten` is absolute
 * and already includes any resume offset — do not add one on top.
 */
export interface UploadEntry {
  username: string;
  filename: string;
  active: boolean;
  position: number;
  size: number;
  bytesWritten: number;
}

/**
 * GET /api/uploads — internal/observ/uploads.go uploadsDTO. Always answers
 * 200; enabled false means the native Soulseek upload manager isn't running
 * (uploads is [] and every counter is 0), the same "enabled, not a failed
 * request" convention as SharesReport. `queued` is the true queue length even
 * when `uploads` was truncated before serialization; `truncated` counts how
 * many queued entries were omitted from `uploads` to keep the payload bounded.
 */
export interface UploadsReport {
  enabled: boolean;
  slots: number;
  active: number;
  queued: number;
  truncated: number;
  uploads: UploadEntry[];
}

/** internal/observ/messages.go messageDTO direction field. */
export type MessageDirection = 'IN' | 'OUT';

/** GET /api/messages — internal/observ/messages.go conversationDTO */
export interface Conversation {
  username: string;
  /** Absent means presence is unknown or unsupported; false is explicitly offline. */
  online?: boolean;
  lastMessage: string;
  lastMessageAt: string;
  lastDirection: MessageDirection;
  unread: number;
  total: number;
}

/**
 * internal/observ/messages.go messageDTO — one private message in a thread.
 *
 * `body` is served verbatim from the peer, never sanitized or escaped on the
 * server (see the Go doc comment on messageDTO) — render it only as a text
 * child, NEVER via dangerouslySetInnerHTML.
 */
export interface PrivateMessage {
  id: number;
  username: string;
  direction: MessageDirection;
  body: string;
  sentAt: string;
  read: boolean;
  admin: boolean;
}

/**
 * GET /api/messages/{username} — internal/observ/messages.go threadResponse.
 * messages is newest-first, matching how ThreadFunc pages backwards through
 * history; hasMore signals whether an older page exists behind the oldest
 * (i.e. last) message in this page.
 */
export interface ThreadPage {
  username: string;
  messages: PrivateMessage[];
  hasMore: boolean;
}

/** POST /api/messages/{username}/read — internal/observ/messages.go markReadResponse. */
export interface MarkReadResult {
  marked: number;
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
 * internal/observ/charts.go throughputSampleDTO — one live throughput sample.
 * The containing field (throughput or uploadThroughput) identifies its
 * direction; activeTransfers is the active count for that direction.
 */
export interface ThroughputSample {
  at: string;
  bytesPerSecond: number;
  activeTransfers: number;
}

/**
 * GET /api/charts — chartsDTO. passes is oldest-first, capped at 20;
 * completedByHour is always exactly 24 zero-filled hourly buckets, oldest
 * first, ending at the current hour; throughput and uploadThroughput are the
 * native soulseek client's recent download and upload samples respectively,
 * oldest first. Both are always [] (never absent) on a non-native backend or
 * when unavailable.
 */
export interface ChartsReport {
  passes: SearchPass[];
  completedByHour: HourCount[];
  throughput: ThroughputSample[];
  uploadThroughput: ThroughputSample[];
}

/**
 * GET /api/stream's `event: live` JSON body — internal/observ/stream.go
 * livePayload. `down` and `up` are always present (the global live rates for
 * their directions, 0 when idle). The directional throughput series used to
 * live on this type too; issue #265 split it onto its own independent
 * `event: throughput` (see ThroughputPayload below) so a subscriber with no
 * sparkline on screen never pays for building or receiving it.
 *
 * `detail` is the whole `GET /api/jobs/{id}/detail` body, present only when
 * the connection was opened with `?job=<id>`, and built server-side by the
 * very same function the REST handler calls. It is therefore used as a
 * *replacement* for the cached detail rather than an overlay on it — issue
 * #258: merging two sources field by field on the client produced four
 * separate regressions in #161, because which source is fresher differs per
 * field and per moment.
 *
 * `jobs` (issue #258, replacing the old partial LiveJob overlay) carries full
 * `Job` wire objects — the same shape `GET /api/jobs` returns per row, built
 * by the same server-side function — but only the ones that *changed* since
 * this subscriber's previous frame; omitted (not empty) when nothing did,
 * matching the Go side's `json:"jobs,omitempty"`. A subscriber that opened
 * the connection with `?jobs=1,2,3` only ever receives entries from that id
 * set; one that didn't (no page to scope to, e.g. Overview) receives every
 * currently live-matched job.
 *
 * Because this is a *delta* of changed jobs, not a snapshot of the live set,
 * a job's absence from one frame does not mean "no live data for it right
 * now" — it means "unchanged since the last frame that mentioned it".
 * api/stream.tsx accumulates each frame's jobs by id (newest entry per id
 * wins, entries persist across frames, reset on reconnect/error) before
 * caching, precisely so that a job which stops changing isn't dropped back
 * to a stale REST value while other jobs keep ticking. Client-side this
 * accumulated set is still a whole-object *replace* by id, never a field
 * merge, for the same reason as `detail` above — see replaceLiveJobs in
 * api/queries.ts.
 */
export interface LivePayload {
  jobs?: WireJob[];
  detail?: JobDetail;
  down: number;
  up: number;
}

/**
 * GET /api/stream's `event: throughput` JSON body — internal/observ/
 * stream.go throughputPayload (issue #265). Sent only to a connection opened
 * with `?throughput=1` (see useThroughputStream in api/stream.tsx); every
 * other subscriber never receives this event at all. `download`/`upload`
 * carry only samples strictly newer than this subscriber's previous
 * throughput frame — the same "send only what's new" contract LivePayload's
 * `jobs` has — but unlike every other field in this app, a sample that
 * doesn't make it into a frame is lost forever rather than self-healing on
 * the next one: see api/queries.ts's mergeThroughputSamples for how the
 * client folds each direction independently into its own cached window
 * anyway (a dropped frame just means a gap in that window, not a wrong
 * value).
 */
export interface ThroughputPayload {
  download?: ThroughputSample[];
  upload?: ThroughputSample[];
}

/**
 * The cached shape of queryKeys.live: a LivePayload plus which job id (if
 * any) the connection was scoped to when this frame arrived. Checked before
 * `detail` is applied so a JobDetail page never adopts a stale previous
 * job's detail during a reconnect after navigating between jobs.
 */
export interface ScopedLivePayload extends LivePayload {
  scopeJobId?: number;
}
