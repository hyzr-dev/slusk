// Every user-facing string lives here. Components must never inline text.
// This is i18n preparation, not i18n — see #86.
export const t = {
  app: {
    name: 'slskdarr',
    tagline: 'Lidarr → Soulseek',
    mark: 'sl',
  },
  nav: {
    overview: 'Overview',
    jobs: 'Jobs',
    events: 'Events',
    peers: 'Peers',
    health: 'Health',
    settings: 'Settings',
  },
  status: {
    queued: 'Queued',
    active: 'Active',
    stalled: 'Stalled',
    done: 'Done',
    failed: 'Failed',
  },
  state: {
    WANTED: 'Wanted',
    SELECTING: 'Selecting candidate',
    DOWNLOADING: 'Downloading',
    IMPORTING: 'Importing',
    DONE: 'Done',
    FAILED: 'Failed',
    CANCELLED: 'Cancelled',
  },
  candidateState: {
    NEW: 'Not tried',
    ACTIVE: 'In progress',
    SUCCEEDED: 'Succeeded',
    FAILED: 'Failed',
  },
  event: {
    search: 'Searched',
    search_fallback: 'Searched (fallback)',
    candidate_selected: 'Candidate selected',
    candidate_rejected: 'Candidate rejected',
    attempt_failed: 'Attempt failed',
    attempt_succeeded: 'Attempt succeeded',
    transfer_stalled: 'Transfer stalled',
    import_ok: 'Import completed',
    import_rejected: 'Import rejected',
    job_failed: 'Job failed',
  },
  columns: {
    album: 'Album / Artist',
    peer: 'Peer',
    progress: 'Progress',
    status: 'Status',
    id: 'ID',
    time: 'Time',
    job: 'Job',
    event: 'Event',
    detail: 'Detail',
    module: 'Module',
    lastRun: 'Last attempt / status',
    score: 'Score',
    succeeded: 'Succeeded',
    failed: 'Failed',
  },
  jobs: {
    detail: 'Job detail',
    searchPlaceholder: 'Search artist, album, peer…',
    allStatuses: 'All',
    empty: 'No jobs match the current filter.',
    back: '← Back',
    cancel: 'Cancel',
    retry: 'Retry',
    attemptHistory: 'Attempt history',
    events: 'Events',
    loading: 'Loading…',
    noAttempts: 'No attempts yet.',
    noEvents: 'No events.',
    cancelFailed: 'Could not cancel the job. It may already have finished.',
    retryFailed: 'Could not retry the job. Only failed jobs can be retried.',
    sleepingUntil: (time: string) => `Sleeping until ${time}`,
    candidates: (tried: number, max: number) => `${tried} of ${max} candidates tried`,
    nextAttempt: (time: string) => `Next attempt: ${time}`,
    retries: (n: number) => `${n} retries`,
    queuePosition: (n: number) => `queue #${n}`,
  },
  events: {
    filterPlaceholder: 'Filter events…',
    empty: 'No events.',
  },
  overview: {
    empty: 'No active downloads.',
    chartPasses: 'Matched albums per pass · last 20',
    chartCompleted: 'Completed downloads · last 24 h',
    noChartData: 'No pass history yet',
    chartRangeStart: '−24 h',
    chartRangeEnd: 'now',
    passesAriaLabel: (matched: number, total: number) =>
      `${matched} of ${total} recent search passes matched`,
    completedAriaLabel: (total: number) =>
      `${total} completed downloads over the last 24 hours`,
    passTooltip: (time: string, matched: number) => `${time} — ${matched} matched`,
  },
  peers: {
    empty: 'No peers recorded yet.',
    noArtistHistory: 'No artist-specific history.',
    artistLine: (id: number, score: string, ok: number, fail: number) =>
      `Artist #${id} — score ${score}, ${ok} succeeded, ${fail} failed`,
  },
  health: {
    neverRun: 'Never run',
    consecutiveFailures: (n: number) => `${n} consecutive failures`,
    empty: 'No modules reported.',
  },
  settings: {
    notWritableNotice:
      'The configuration file is mounted read-only, so settings cannot be edited here. Mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount to enable editing.',
    lidarr: 'Lidarr',
    slskd: 'slskd',
    url: 'URL',
    apiKey: 'API key',
    // Generic write-only-secret placeholders, reused across all six secret
    // fields (lidarr/slskd API keys, Soulseek password, gluetun API key,
    // store DSN, observ auth token) — never resent, only "is one set?".
    secretPlaceholderConfigured: 'Configured — leave blank to keep',
    secretPlaceholderMissing: 'Not configured',
    pipeline: 'Pipeline',
    backend: 'Backend',
    backendSlskd: 'slskd',
    backendSoulseek: 'Soulseek (native)',
    maxCandidatesPerAlbum: 'Max candidates per album',
    maxActive: 'Max active jobs',
    maxRetries: 'Max retries',
    maxInflightPerPeer: 'Max inflight per peer',
    maxTransferRetries: 'Max transfer retries',
    minBitrate: 'Minimum bitrate (kbps)',
    transferDeadline: 'Transfer deadline',
    stallTimeout: 'Stall timeout',
    searchTimeout: 'Search timeout',
    backoffBase: 'Backoff base',
    backoffCap: 'Backoff cap',
    candidateTtl: 'Candidate TTL',
    failedReviveAfter: 'Failed revive after',
    stuckAfter: 'Stuck after',
    tickTimeout: 'Tick timeout',
    importConfirmTimeout: 'Import confirm timeout',
    wantedSyncInterval: 'Wanted sync interval',
    discoveryInterval: 'Discovery interval',
    selectingInterval: 'Selecting interval',
    downloadingInterval: 'Downloading interval',
    importingInterval: 'Importing interval',
    manualImportTimeout: 'Manual import timeout',
    importRetryCooldown: 'Import retry cooldown',
    weights: 'Matching weights',
    weightFormat: 'Format weight',
    weightBitrate: 'Bitrate weight',
    weightReliability: 'Reliability weight',
    weightFileCount: 'File count weight',
    weightKnownUser: 'Known-user weight',
    soulseek: 'Soulseek',
    serverAddress: 'Server address',
    username: 'Username',
    password: 'Password',
    listenAddr: 'Listen address',
    uploadSlots: 'Upload slots',
    gluetunTitle: 'Gluetun (VPN port forwarding)',
    gluetunControlUrl: 'Control URL',
    sharedFoldersTitle: 'Shared folders',
    folderName: 'Name',
    folderPath: 'Path',
    addFolder: 'Add folder',
    removeFolder: 'Remove',
    observability: 'Observability',
    logLevel: 'Log level',
    logLevelDefault: 'Default (info)',
    logLevelDebug: 'Debug',
    logLevelInfo: 'Info',
    logLevelWarn: 'Warn',
    logLevelError: 'Error',
    dangerZone: 'Danger zone',
    dsn: 'Database connection string (DSN)',
    authToken: 'Auth token',
    slskdCompleteDir: 'Completed downloads directory',
    dangerConfirmWarning:
      'Click Save again to confirm — this changes the database connection, listen address, auth token, or download path. If you change the listen address or auth token, this page may not reconnect automatically after the restart — you may need to reload and sign in again.',
    saveConfirm: 'Click again to confirm',
    dangerRecoveryHint:
      'slskdarr writes the previous configuration to config.toml.bak before saving. Restoring it requires editing the file directly and a manual restart.',
    // Real TOML keys (config.example.toml), shown alongside the
    // human-readable labels above so a user can find the setting they see
    // here in the file they'd actually edit. Bare key names (not dotted with
    // their section) since the enclosing card already names the section.
    configKeys: {
      lidarrUrl: 'url',
      lidarrApiKey: 'api_key',
      slskdUrl: 'url',
      slskdApiKey: 'api_key',
      backend: 'backend',
      maxCandidatesPerAlbum: 'max_candidates_per_album',
      maxActive: 'max_active',
      maxRetries: 'max_retries',
      maxInflightPerPeer: 'max_inflight_per_peer',
      maxTransferRetries: 'max_transfer_retries',
      minBitrate: 'min_bitrate',
      transferDeadline: 'transfer_deadline',
      stallTimeout: 'stall_timeout',
      searchTimeout: 'search_timeout',
      backoffBase: 'backoff_base',
      backoffCap: 'backoff_cap',
      candidateTtl: 'candidate_ttl',
      failedReviveAfter: 'failed_revive_after',
      stuckAfter: 'stuck_after',
      tickTimeout: 'tick_timeout',
      importConfirmTimeout: 'import_confirm_timeout',
      wantedSyncInterval: 'wanted_sync_interval',
      discoveryInterval: 'discovery_interval',
      selectingInterval: 'selecting_interval',
      downloadingInterval: 'downloading_interval',
      importingInterval: 'importing_interval',
      manualImportTimeout: 'manual_import_timeout',
      importRetryCooldown: 'import_retry_cooldown',
      weightFormat: 'format',
      weightBitrate: 'bitrate',
      weightReliability: 'reliability',
      weightFileCount: 'file_count',
      weightKnownUser: 'known_user',
      serverAddress: 'server_address',
      username: 'username',
      password: 'password',
      soulseekListenAddr: 'listen_addr',
      uploadSlots: 'upload_slots',
      gluetunControlUrl: 'control_url',
      gluetunApiKey: 'api_key',
      sharedFolders: 'shared_folders',
      logLevel: 'log_level',
      dsn: 'dsn',
      observListenAddr: 'listen_addr',
      authToken: 'auth_token',
      slskdCompleteDir: 'slskd_complete_dir',
    },
    connections: 'Connections',
    testConnection: 'Test',
    testStatus: {
      untested: 'Not tested',
      testing: 'Testing…',
      success: 'Connected',
      failure: 'Failed',
    },
    testUnreachable: 'The test endpoint could not be reached.',
    testFailed: 'Connection test failed.',
    // Client-side checks for numeric fields (see Settings.tsx numericFieldErrors) —
    // everything else is validated server-side only.
    fieldRequired: 'Required',
    mustBeWholeNumber: 'Must be a whole number',
    save: 'Save',
    saving: 'Saving…',
    savedRestarting: 'Saved — restarting…',
    saveFailed: 'Could not save the configuration. Please try again.',
  },
} as const;

// `as const` narrows the maps below to literal-key object types, so indexing
// them with a plain `string` (e.g. a backend enum value that isn't known at
// compile time) fails under --strict with TS7053. These helpers are the
// sanctioned way to do that dynamic lookup: unlike `MAP[key] || key` in the
// legacy dashboard, they stay type-safe and degrade to the raw key when the
// backend sends a code we don't recognise, instead of rendering blank.
function lookup<T extends Record<string, string>>(map: T, key: string): string | undefined {
  return Object.prototype.hasOwnProperty.call(map, key) ? map[key as keyof T] : undefined;
}

/** Label for a job event code, falling back to the raw code if unrecognised. */
export function eventLabel(code: string): string {
  return lookup(t.event, code) ?? code;
}

/** Label for a candidate state, falling back to the raw state if unrecognised. */
export function candidateStateLabel(state: string): string {
  return lookup(t.candidateState, state) ?? state;
}

/**
 * Label for a job state, with a deliberately asymmetric fallback: an unknown
 * state degrades to the *translated status* (the coarser field), and only an
 * unknown status falls back to its raw value. This mirrors the legacy
 * dashboard's `STATE_LABEL[j.state] || j.status`, which preferred the coarser
 * status over a raw enum string.
 */
export function stateLabel(state: string, status: string): string {
  return lookup(t.state, state) ?? lookup(t.status, status) ?? status;
}
