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
  },
  events: {
    filterPlaceholder: 'Filter events…',
    empty: 'No events.',
  },
  overview: {
    empty: 'No active downloads.',
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
    readOnlyNotice:
      'Settings are read from the configuration file. Editing them here is planned — see issue #89.',
    lidarr: 'Lidarr',
    url: 'URL',
    apiKey: 'API key',
    apiKeyHidden: 'Configured (hidden)',
    apiKeyMissing: 'Not configured',
    pipeline: 'Pipeline',
    wantedSyncInterval: 'Wanted sync interval',
    maxActive: 'Max active jobs',
    // Real TOML keys (config.example.toml [pipeline]), shown alongside the
    // human-readable labels above so a user can find the setting they see
    // here in the file they'd actually edit.
    configKeys: {
      wantedSyncInterval: 'wanted_sync_interval',
      maxActive: 'max_active',
    },
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
