// Hand-written mirrors of the Go DTOs in internal/observ. Kept in one file so
// drift has a single place to be caught. See spec 2026-07-20.

// Canonical UI values. Legacy wire values are kept separate below and
// normalized before React Query caches a response. 'importing' is a real
// per-job status now (issue #269) — the backend used to serialize an
// IMPORTING job's status as 'active' (Tag derived the IM tag separately from
// `state`), a drift between the SQL and Go copies of this rule that this
// value removes the need for. 'notImported' (issue #59) is the terminal
// state of a manual job that finished downloading with no Lidarr album to
// import into — it downloaded successfully and was never handed to Lidarr,
// which is neither a success nor a failure and must not read as either (see
// Tag's TONE).
export type JobStatus = 'queued' | 'active' | 'stalled' | 'importing' | 'done' | 'failed' | 'parked' | 'notImported';
// NOT_IMPORTED is terminal. JobActions.TERMINAL_STATES must list it: the
// store's cancel path rewrites a job's state unconditionally, so hiding the
// Cancel button is the only thing stopping a terminal job from being
// rewritten to CANCELLED.
export type JobState =
  | 'WANTED' | 'SELECTING' | 'DOWNLOADING' | 'IMPORTING'
  | 'DONE' | 'FAILED' | 'CANCELLED' | 'PARKED' | 'NOT_IMPORTED';
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
 * 'failures' is Overview's third region filter (issue #310, review
 * follow-up): every job whose STATE is FAILED, time-unbounded. It is
 * deliberately distinct from 'failed' below — 'failed' is status-derived
 * (dashboardJobStatusSQL) and also matches a job still DOWNLOADING whose
 * current candidate merely errored and will be retried; 'failures' is
 * state-keyed and excludes that job, the same way 'inflight'/'finished' are
 * kept disjoint from each other.
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
  | 'finished'
  | 'failures';
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
  // The pipeline's own last recorded failure explanation from job_events (see
  // internal/observ jobDTO.FailDetail), typically Lidarr's verbatim rejection
  // text. Unlike failReason (the current candidate's generic category), this
  // is populated only by GET /api/jobs, never by the live stream — so it's
  // optional and absent on stream-sourced jobs.
  failDetail?: string;
  nextAttemptAt: string;
  retries: number;
  notBefore: string;
  source: JobSource;
  year: number | null;
  tracks: number | null;
  format: string | null;
  // The MusicBrainz release-group MBID this job was created with, if any
  // (issue #59) — absent means the job was posted without one and can never
  // reach Lidarr import (see `notImported` on JobStatus).
  albumMbid?: string;
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
 *
 * `code` is a machine-readable discriminator, present on error bodies that
 * need one to be handled distinctly from an ordinary failure — currently
 * only POST /api/lidarr/artists' 502 `"addUncertain"` (see
 * AddLidarrArtistResult's doc comment): that status means the add may or may
 * not have happened, which IdentifyModal must not render as a definite
 * failure the way every other status does.
 */
export interface ApiErrorBody {
  error: string;
  fieldErrors?: Record<string, string>;
  code?: string;
}

/**
 * GET /api/auth/session — internal/observ/auth.go sessionResponse (issue
 * #279). `username` is null both while unauthenticated and when the request
 * carried the machine bearer token instead of a browser session cookie — see
 * that file's doc comment on registerAuth (this is the `make dev` case: the
 * Vite proxy injects a bearer token, so the dev server reports
 * authenticated:true, username:null even against a lab DB with zero users).
 * `setupRequired` is independent of `authenticated` — it reflects whether any
 * account exists at all, not whether this particular request is one.
 */
export interface SessionResponse {
  authenticated: boolean;
  username: string | null;
  setupRequired: boolean;
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

/**
 * internal/core/uploads.go UploadStatus — how one upload to a peer ended.
 * 'rejected' means nothing was ever streamed (the peer asked for a file we
 * would not or could not serve), which is why a rejected row's bytesSent and
 * avgBytesPerSecond are 0 as a true measurement rather than a missing one.
 */
export type UploadHistoryStatus = 'completed' | 'aborted' | 'rejected';

/**
 * internal/observ/uploads.go uploadHistoryDTO — one finished upload.
 * `filename` is the backslash-separated virtual share path (see
 * formatVirtualPath); `detail` is a short fixed reason string, never a raw
 * error, and is empty when there is nothing to say.
 */
export interface UploadHistoryEntry {
  id: number;
  username: string;
  filename: string;
  size: number;
  bytesSent: number;
  avgBytesPerSecond: number;
  status: UploadHistoryStatus;
  detail: string;
  startedAt: string;
  finishedAt: string;
}

/**
 * GET /api/uploads/history — internal/observ/uploads.go
 * uploadHistoryResponse. Newest-first (id DESC); `hasMore` is computed by
 * over-fetching one row, so it is exact rather than a guess. Unlike
 * UploadsReport this carries no `enabled` flag: the history is a fact in the
 * database, readable after the native backend is switched off — though
 * Shares.tsx currently mounts <UploadHistory /> only inside its `data.enabled`
 * branch, so that survival is not yet surfaced in the UI.
 */
export interface UploadHistoryPage {
  uploads: UploadHistoryEntry[];
  hasMore: boolean;
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

// ---- Manual search (issue #58) ----

/**
 * internal/observ/search.go searchFileDTO — one file within a search group.
 * `filename` is the full peer-syntax path — exactly what POST /api/jobs
 * requires to enqueue it — while `name` is its display basename. Every
 * optional attribute mirrors the Go DTO's `omitempty`: absent means the peer
 * sent no such attribute (see core.SearchResult's doc comment), so the
 * honest type here is `number | undefined`, never a misleading zero.
 */
export interface WireSearchFile {
  filename: string;
  name: string;
  size: number;
  bitrate?: number;
  durationSeconds?: number;
  sampleRate?: number;
  bitDepth?: number;
  variableBitRate?: boolean;
}

/**
 * internal/observ/search.go searchGroupDTO — one release folder offered by
 * one peer, grouped server-side by (username, ReleaseDir) so this grouping
 * logic never has to be reimplemented in TypeScript (issue #58 §4/§5).
 * `parent` is the peer's parent folder name, not a resolved artist — that
 * fact isn't on the Soulseek wire at all, see the same section.
 */
export interface WireSearchGroup {
  id: string;
  peer: string;
  folder: string;
  title: string;
  parent: string;
  trackCount: number;
  sizeBytes: number;
  durationSeconds?: number;
  format?: string;
  bitrate?: number;
  sampleRate?: number;
  bitDepth?: number;
  variableBitRate?: boolean;
  freeUploadSlot: boolean;
  queueLength: number;
  uploadSpeed: number;
  score: number;
  files: WireSearchFile[];
}

/**
 * internal/observ/search.go searchSessionDTO — served at POST /api/search
 * (201) and GET /api/search/{id} (200), byte-identical in shape between the
 * two so the frontend needs exactly one normalizer for both transports.
 */
export interface WireSearchSession {
  id: string;
  query: string;
  startedAt: string;
  done: boolean;
  streaming: boolean;
  truncated?: boolean;
  error?: string;
  total: number;
  groups: WireSearchGroup[];
}

// Normalized shapes. Identical to their Wire counterparts today — unlike Job,
// this DTO has no orphaned/parked-style enum drift to correct — but kept as
// distinct aliases (rather than reusing the Wire types directly) so
// api/queries.ts and the Search route read the same "normalized" vocabulary
// every other resource in this file uses, and a future divergence has
// somewhere to go without touching every call site.
export type SearchFile = WireSearchFile;
export type SearchGroup = WireSearchGroup;

/**
 * The session as cached, which is the wire shape plus two client-owned fields
 * the server neither sends nor could send — this is the divergence the aliases
 * above were kept distinct for.
 *
 * `streamedAt` is the local clock reading of the last `event: search` frame
 * folded into this object (see replaceSearchGroups). It exists so
 * useSearchSession can arm its fallback poll on *silence* rather than on the
 * SSE connection's nominal state: a connection that is open but delivering
 * nothing is indistinguishable from a healthy one at the EventSource level,
 * and it is the state the fallback exists for. Never compared against any
 * server timestamp — it is a client clock reading and only ever differenced
 * against Date.now().
 *
 * `expired` records that the server evicted the session before it finished.
 * The wire says so exactly once, on the frame that reports it (SearchPayload's
 * `expired`), and that frame also forces `done: true` — so without keeping the
 * distinction here the view would call an evicted, partial session "search
 * complete".
 */
export interface SearchSession extends WireSearchSession {
  streamedAt?: number;
  expired?: boolean;
}

/**
 * POST /api/search request body — internal/observ/search.go
 * createSearchRequest.
 */
export interface CreateSearchRequest {
  query: string;
}

// ---- MusicBrainz identify (issue #321) ----

/**
 * GET /api/identify/search — internal/observ identify.go, one combined
 * artist+release-group match. Replaces what was originally two endpoints
 * (an artist search, then that artist's album list) — see the doc comment
 * on IdentifyModal's searchMB for why the combined shape is the one that
 * ships. `editionCount` is a genuine single-call field (the release-group
 * search's own `count`), not a second per-row lookup — it fills the mock's
 * EDITIONS column directly. `score` is MusicBrainz's own relevance score
 * (0-100); results already arrive ranked by it, so the frontend never
 * re-sorts.
 *
 * `artist`/`artistId` are `omitempty` on the Go DTO (mbSearchResultDTO) and
 * genuinely absent — not empty strings — when a release-group's
 * artist-credit is empty. Render their absence as absent (never a
 * placeholder or "undefined"), per the project's "never invent data" rule —
 * see IdentifyModal's suggestion row and its confirm() fallback.
 */
export interface MusicBrainzSearchResult {
  id: string;
  title: string;
  artist?: string;
  artistId?: string;
  primaryType?: string;
  secondaryTypes: string[];
  firstReleaseDate?: string;
  editionCount: number;
  score: number;
}

/**
 * GET /api/identify/search response. `album` is required server-side (a
 * blank one 422s before any upstream call); `artist` is optional. `total`
 * is the true relevance-ranked hit count and routinely far exceeds
 * `results.length` (a query can rank hundreds of loosely-related releases) —
 * this is NOT a paginated catalogue the way the albums/editions endpoints
 * are, so IdentifyModal's truncation notice for this response reads
 * "showing the best N matches" rather than "showing N of total".
 */
export interface MusicBrainzSearchResponse {
  results: MusicBrainzSearchResult[];
  total: number;
}

/**
 * GET /api/identify/albums/{mbid}/editions — one release (a specific
 * pressing/edition) belonging to a release-group. `trackCountKnown: false`
 * means MusicBrainz has no track listing for this release — render that as
 * unknown, never as a 0-track edition (see the brief's "never render it as
 * 0" rule).
 */
export interface MusicBrainzEdition {
  id: string;
  title: string;
  date?: string;
  country?: string;
  status?: string;
  trackCount: number;
  trackCountKnown: boolean;
}

/** GET /api/identify/albums/{mbid}/editions response. `total` is the true count; `editions` is capped at 100. */
export interface MusicBrainzEditionListResult {
  editions: MusicBrainzEdition[];
  total: number;
}

/**
 * GET /api/identify/albums/{mbid}/lidarr — whether Lidarr already knows this
 * release-group. `known: false` means the check itself is unavailable
 * (Lidarr unreachable), which is a distinct state from "known and not in the
 * library" — see the three-way LIDARR STATUS copy this drives.
 */
export interface LidarrMatch {
  known: boolean;
  inLibrary: boolean;
  albumId?: number;
}

// ---- Lidarr add-artist flow (issue #331) ----

/**
 * GET /api/lidarr/artists/{mbid} — internal/observ/lidarr.go
 * lidarrArtistStatusDTO. Whether Lidarr already knows this MusicBrainz
 * ARTIST, the artist-level counterpart to LidarrMatch above (which is
 * per-album). Same three-way semantics: `known: false` means the check
 * itself is unavailable, never "not in library". `artistId`/`name` are
 * `omitempty` and genuinely absent when the artist is not in the library or
 * the check failed.
 */
export interface LidarrArtistMatch {
  known: boolean;
  inLibrary: boolean;
  artistId?: number;
  name?: string;
}

/** One root folder from GET /api/lidarr/add-options — internal/observ/lidarr.go lidarrRootFolderDTO. */
export interface LidarrRootFolder {
  id: number;
  path: string;
  accessible: boolean;
  freeSpace: number;
  defaultQualityProfileId: number;
  defaultMetadataProfileId: number;
}

/** One quality or metadata profile from GET /api/lidarr/add-options — internal/observ/lidarr.go lidarrProfileDTO. */
export interface LidarrProfile {
  id: number;
  name: string;
}

/**
 * GET /api/lidarr/add-options response — the "add to Lidarr" form's root
 * folders and profiles, fetched only once that path is opened (see
 * IdentifyModal's openAddFlow).
 */
export interface LidarrAddOptions {
  rootFolders: LidarrRootFolder[];
  qualityProfiles: LidarrProfile[];
  metadataProfiles: LidarrProfile[];
}

/** POST /api/lidarr/artists' `monitor` field — "album" for just the release that prompted the add, "all" for the whole discography. */
export type LidarrMonitorChoice = 'album' | 'all';

/** POST /api/lidarr/artists request body — internal/observ/lidarr.go addArtistRequest. */
export interface AddLidarrArtistRequest {
  artistMbid: string;
  artistName: string;
  albumMbid: string;
  rootFolderPath: string;
  qualityProfileId: number;
  metadataProfileId: number;
  monitor: LidarrMonitorChoice;
}

/**
 * `albumMonitorState` values for AddLidarrArtistResult, in the order
 * IdentifyModal treats them as increasingly uncertain:
 *  - 'monitored': the album is confirmed monitored — the ordinary case.
 *  - 'notVisibleYet': the artist was created but Lidarr had not finished
 *    refreshing it yet, so the album could not be monitored.
 *  - 'reverted': the album was monitored immediately after the add, but
 *    Lidarr's own refresh then reset it — it did not stick.
 *  - 'unknown': the monitoring state could not be confirmed at all. This is
 *    NOT a failure and must never be rendered as one — see IdentifyModal's
 *    copy for this case.
 */
export type LidarrAlbumMonitorState = 'monitored' | 'notVisibleYet' | 'reverted' | 'unknown';

/**
 * POST /api/lidarr/artists' 201 response — internal/observ/lidarr.go
 * addArtistResultDTO. Neither `artistMonitored: false` nor any
 * `albumMonitorState` other than 'monitored' is a failure — the artist (and
 * usually the album) were still created; see IdentifyModal's per-state copy
 * for what each combination means to the user.
 *
 * A 502 response to this same endpoint, with `{"code": "addUncertain"}` in
 * its ApiErrorBody, is a different case again: the add may or may not have
 * happened at all. That is carried as an HTTP error, not a value of this
 * type — see IdentifyModal's submitAddArtist.
 */
export interface AddLidarrArtistResult {
  artistId: number;
  alreadyInLibrary: boolean;
  /** Whether the artist is monitored in Lidarr now. */
  artistMonitored: boolean;
  albumMonitorState: LidarrAlbumMonitorState;
}

/**
 * POST /api/jobs request body — internal/observ/observ.go createJobRequest
 * (issue #155). A manual job that downloads known files directly from one
 * peer, bypassing Discovery/Selecting entirely. Title/artist are optional
 * free-text display fields; peer and at least one file are required. The
 * 201 response is a plain jobDTO — the same shape GET /api/jobs already
 * returns per row — so it reuses WireJob rather than a new type.
 */
export interface CreateJobRequest {
  title: string;
  artist: string;
  peer: string;
  files: { filename: string; size: number }[];
  // The MusicBrainz release-group MBID to import into Lidarr once the
  // download finishes (issue #59) — a lowercase 8-4-4-4-12 hex UUID. Omitted
  // (or blank) means the job is downloaded but deliberately never imported;
  // the backend resolves this to a Lidarr album at import time rather than
  // trusting whatever Lidarr ids were resolved during identify, so a
  // transient Lidarr outage at identify time never has to downgrade a job.
  albumMbid?: string;
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

/**
 * GET /api/stream's `event: search` JSON body — internal/observ/stream.go
 * searchPayload (issue #58). Sent only to a connection opened with
 * `?search=<id>` (see useSearchStream in api/stream.tsx). `groups` carries
 * only whole groups that changed since this subscriber's previous frame —
 * never a file-level diff, so the (username, ReleaseDir) grouping stays
 * server-side (see WireSearchGroup's doc comment) — omitted (not empty) when
 * nothing changed, matching the Go side's `json:"groups,omitempty"`. `seq`
 * is bookkeeping for the server's own per-subscriber cursor; the frontend
 * never reads it back.
 *
 * `expired` is set, with every other field at its zero value except `id`,
 * when the requested session id is well-formed but no longer known — evicted
 * between the POST and this connection, or a reconnect landing on a session
 * that has since finished and aged out. It is deliberately not carried as an
 * HTTP error: a stale-but-well-formed id is a routine outcome, not a
 * malformed request.
 */
export interface SearchPayload {
  id: string;
  seq: number;
  groups?: WireSearchGroup[];
  total: number;
  done: boolean;
  streaming: boolean;
  truncated?: boolean;
  expired?: boolean;
  error?: string;
}
