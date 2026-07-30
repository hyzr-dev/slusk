// Every user-facing string lives here. Components must never inline text.
// This is i18n preparation, not i18n — see #86.
const CHAT_ONLINE = 'ONLINE';
const CHAT_OFFLINE = 'OFFLINE';
const CHAT_STATUS_ONLINE = 'online';
const CHAT_STATUS_OFFLINE = 'offline';

export const t = {
  app: {
    name: 'slskdarr',
    tagline: 'Lidarr → Soulseek',
    mark: 'sl',
  },
  nav: {
    overview: 'overview',
    jobs: 'jobs',
    events: 'events',
    peers: 'peers',
    health: 'health',
    search: 'search',
    shares: 'shares',
    chat: 'chat',
    setup: 'setup',
    settings: 'config',
    groupMonitor: 'MONITOR',
    groupSoulseek: 'SOULSEEK',
    groupSystem: 'SYSTEM',
  },
  // Each route's <Page> title and subtitle (the 27 July TUI restyle, #281).
  // Title case here, unlike `nav` above: the sidebar keeps the mock's
  // lowercase terminal idiom, but a page's own <h1> reads as ordinary prose.
  // Shares and Settings deliberately don't reuse their nav/settings labels —
  // the mock titles those pages "Sharing" and "Config" respectively.
  page: {
    overview: {
      title: 'Overview',
      subtitle: 'Lidarr wanted list, reconciled against Soulseek every 45s',
    },
    jobs: {
      title: 'Jobs',
      subtitle: 'Every album slskdarr is tracking, from queued to imported',
    },
    search: {
      title: 'Search',
      subtitle: 'Query the Soulseek network directly and import straight into Lidarr',
    },
    health: {
      title: 'Health',
      subtitle: 'Dependencies, reconcile throughput and raw metrics',
    },
    shares: {
      title: 'Sharing',
      subtitle: 'What you give back to the network — and who is pulling from you',
    },
    chat: {
      title: 'Chat',
      subtitle: 'Direct messages with peers — useful when a transfer needs a nudge',
    },
    setup: {
      title: 'Setup',
      subtitle: 'slskdarr validates the config you already wrote — it never writes it',
    },
    settings: {
      title: 'Config',
      subtitle: 'Resolved runtime configuration, as slskdarr sees it',
    },
    // Not in the mock (docs/design/slskdarr-tui.dc.html drops both views),
    // but Events and Peers are still shipped — see Page.tsx's doc comment.
    // Titles/subtitles are invented here in the same voice as the rest of
    // this object rather than left unstyled.
    events: {
      title: 'Events',
      subtitle: 'The raw job-event log across every job, newest first',
    },
    peers: {
      title: 'Peers',
      subtitle: 'Everyone slskdarr has downloaded from, ranked by reliability',
    },
  },
  chrome: {
    live: 'LIVE',
    // Shown in place of LIVE once polling has visibly stopped keeping up, so a
    // number in this cell always means something is wrong. While healthy the
    // cell carries no digits at all — a counter that resets every few seconds
    // is noise 99% of the time and trains the eye to ignore it.
    stale: (age: string) => `STALE ${age}`,
    reconcile: 'RECONCILE',
    reconcileNever: '—',
    down: 'DOWN',
    up: 'UP',
    throughputSeparator: '·',
    idle: 'idle',
    statusRegion: 'Application status',
  },
  // Shared query-state copy. Every view renders the same three states for a
  // GET — loading, failed, stale — so the two that are not view-specific live
  // here instead of being borrowed from whichever namespace happened to
  // declare one first (issue #201; this replaces jobs.loading, which five
  // non-jobs views were reading).
  query: {
    loading: 'Loading…',
    // No endpoint named on purpose: the region showing this line is already
    // labelled, and a URL is not something the reader can act on.
    failed: 'Could not load.',
    // Distinct from `failed` because the two say different things: this one
    // appears above data that is still on screen and still true as of the
    // last successful poll (see App.tsx on keeping last-known data, #87).
    stale: 'Could not refresh — showing last known data.',
  },
  status: {
    queued: 'Queued',
    active: 'Active',
    stalled: 'Stalled',
    importing: 'Importing',
    done: 'Done',
    failed: 'Failed',
    parked: 'Parked',
  },
  // Two-letter status tags in the TUI job grid. The long labels in `status`
  // and `state` are still used wherever there is room for them.
  tag: {
    DL: 'DL',
    QU: 'QU',
    ST: 'ST',
    PA: 'PA',
    FA: 'FA',
    OK: 'OK',
    IM: 'IM',
    // Uploads panel marker, not a JobStatus/JobState — the map already
    // serves as a general two-letter tag vocabulary, so it's added here
    // rather than duplicated in its own small map.
    UL: 'UL',
  },
  tagTitle: {
    DL: 'Downloading',
    QU: 'Queued',
    ST: 'Stalled',
    PA: 'Parked',
    FA: 'Failed',
    OK: 'Done',
    IM: 'Importing',
    UL: 'Uploading',
  },
  state: {
    WANTED: 'Wanted',
    SELECTING: 'Selecting candidate',
    DOWNLOADING: 'Downloading',
    IMPORTING: 'Importing',
    DONE: 'Done',
    FAILED: 'Failed',
    CANCELLED: 'Cancelled',
    PARKED: 'Parked',
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
  // The generic column-label object that predates this reskin. Each reskinned
  // view now owns its own `gridHead` map matching the mock's column names;
  // only the labels still shared by not-yet-reskinned views remain here.
  columns: {
    status: 'Status',
    time: 'Time',
    job: 'Job',
    event: 'Event',
    detail: 'Detail',
  },
  source: {
    manual: 'Manual',
    lidarr: 'Lidarr',
  },
  jobs: {
    searchPlaceholder: 'Search artist, album, peer…',
    noMatch: 'No jobs match the filter.',
    back: '← Back',
    cancel: 'Cancel',
    retry: 'Retry',
    parkedExplanation:
      'Repeated backend disappearance exhausted transfer retries, so automation stopped. Retry discards prior candidates and transfers, then starts a fresh search and download cycle.',
    forceSearch: 'Force search',
    delete: 'Delete',
    deleteConfirm: 'Click again to delete',
    // Upper-cased here rather than in CSS — see SectionHeader.
    attemptHistory: 'ATTEMPT HISTORY',
    events: 'EVENTS',
    noAttempts: 'No attempts yet.',
    noEvents: 'No events.',
    noCandidate: 'No candidate yet.',
    cancelFailed: 'Could not cancel the job. It may already have finished.',
    retryFailed: 'Could not retry the job. Only failed or parked jobs can be retried.',
    forceSearchFailed: 'Could not force a search. The job may already be active.',
    deleteFailed: 'Could not delete the job. It may currently be importing.',
    sleepingUntil: (time: string) => `Sleeping until ${time}`,
    candidates: (tried: number, max: number) => `${tried} of ${max} candidates tried`,
    nextAttempt: (time: string) => `Next attempt: ${time}`,
    retries: (n: number) => `${n} retries`,
    queuePosition: (n: number) => `queue #${n}`,
    // The attempt header's file count (job detail page) and a transfer's own
    // retry count — both were inline template strings before this reskin.
    fileCount: (n: number) => `${n} files`,
    transferRetries: (n: number) => `${n} retries`,
    verifying: 'verifying',
    showDetails: 'Show details',
    hideDetails: 'Hide details',
    moreFiles: (n: number) => `+${n} more files`,
    // FILES is the only titled column in the TUI expansion (mock,
    // docs/design/slskdarr-tui.dc.html:190) — the meta column on the left has
    // no heading of its own.
    files: 'FILES',
    // The expansion's left-column meta tree (mock line ~1069): lowercase,
    // terminal-style labels rather than the Title Case used elsewhere in the
    // app, matching that mock's wording exactly.
    peerLabel: 'peer',
    sourceLabel: 'source',
    queuePositionLabel: 'queue pos',
    // The meta row's value when a job is genuinely waiting in a peer's queue
    // (mock: '#'+queuePos+' in peer queue').
    queuePositionMeta: (n: number) => `#${n} in peer queue`,
    timeInStateLabel: 'time in state',
    qualityLabel: 'quality',
    transferredLabel: 'transferred',
    jobIdLabel: 'job id',
    // The TUI job grid's column headers (mock line 162) — short forms
    // distinct from the generic Title Case labels in `columns`, which other
    // (not yet reskinned) views still use.
    gridHead: {
      status: 'ST',
      album: 'ALBUM',
      peer: 'PEER',
      format: 'FMT',
      progress: 'PROGRESS',
      speed: 'SPEED',
      eta: 'ETA',
      tries: 'TRY',
    },
    // Filter chip labels use the canonical UI state names.
    chipLabel: {
      all: 'ALL',
      active: 'ACTIVE',
      queued: 'QUEUED',
      stalled: 'STALLED',
      failed: 'FAILED',
      parked: 'PARKED',
      done: 'DONE',
    },
    // A second, orthogonal chip row (Manual vs Lidarr-sourced jobs) — not in
    // the mock, but source filtering is an approved Jobs control. The group's
    // own accessible name —
    // distinct from sourceLabel above, which is the expansion meta-tree's
    // (lowercase) row label and must stay free to reword independently.
    sourceFilterLabel: 'Source',
    sourceChipLabel: {
      all: 'ALL',
      manual: 'MANUAL',
      lidarr: 'LIDARR',
    },
    paginationLabel: 'Job pages',
    previousPage: '[,] PREV',
    nextPage: 'NEXT [.]',
    pageLabel: (page: number) => `Page ${page}`,
    resultRange: (start: number, end: number, total: number) => `${start}–${end} of ${total} jobs`,
    // Compact peer-queue position for the dense PROGRESS cell, where "queue
    // #4" (queuePosition above) would overflow the column.
    queueShort: (n: number) => `P${n}`,
    // Flash confirmations (see FlashContext) for the three row actions —
    // mutations the row itself won't visibly reflect before the next poll.
    retryFlash: (id: number) => `retried #${id}`,
    cancelFlash: (id: number) => `cancelled #${id}`,
    forceSearchFlash: (id: number) => `search forced for #${id}`,
    deleteFlash: (id: number) => `deleted #${id}`,
  },
  events: {
    filterPlaceholder: 'Filter events…',
    empty: 'No events.',
  },
  overview: {
    empty: 'No active downloads.',
    noChartData: 'No pass history yet',
    chartRangeStart: '−24 h',
    chartRangeEnd: 'now',
    passesAriaLabel: (matched: number, total: number) =>
      `${matched} of ${total} recent search passes matched`,
    completedAriaLabel: (total: number) =>
      `${total} completed downloads over the last 24 hours`,
    passTooltip: (time: string, matched: number) => `${time} — ${matched} matched`,
    // TUI Overview page (#198): the section headers above the TRANSFERS and
    // THROUGHPUT panels. Uppercase in source, per SectionHeader's contract —
    // RECONCILE reuses t.chrome.reconcile rather than a third copy of the word.
    transfersHeading: 'TRANSFERS',
    throughputHeading: 'THROUGHPUT',
    inFlightCountMeta: (n: number) => `${n} in flight`,
    // Shown instead of inFlightCountMeta when the panel cannot fit every
    // in-flight job: max_active can exceed the panel's row count, and silently
    // dropping the remainder would read as "this is all of it".
    inFlightTruncatedMeta: (shown: number, total: number) => `${shown} of ${total} in flight`,
    // The peer-queue special case: an "active" job with no bytes moving.
    queuePos: (n: number) => `queue pos ${n}`,
    // The dense TRANSFERS table's SIZE column for the same case (mock: 'pos
    // '+queuePos) — shorter than queuePos above because it shares a column
    // with a byte count, not a full sentence.
    queuePosShort: (n: number) => `pos ${n}`,
    // The four stat cells (#281 restyle) — 'IMPORTED 24H' rather than the
    // mock's 'IMPORTED TODAY': there is no calendar-day counter, only
    // /api/charts' completedByHour, a rolling 24h window (see Overview.tsx).
    statInFlight: 'In flight',
    statQueued: 'Queued',
    statImported: 'Imported 24h',
    statAttention: 'Needs attention',
    subInFlight: (peers: number) => `downloading from ${peers} peers`,
    subQueued: 'awaiting a free slot',
    subImported: (n: number) => `${n} verifying now`,
    subAttention: (stalled: number, parked: number) => `${stalled} stalled · ${parked} parked`,
    // The TRANSFERS table's column headers (mock line 103).
    gridHead: {
      status: 'ST',
      album: 'ALBUM',
      peer: 'PEER',
      progress: 'PROGRESS',
      speed: 'SPEED',
      size: 'SIZE',
    },
    // The RECONCILE table's column headers. Three, not the mock's four
    // (mock line 135 has WHEN/PASS/RESULT/DUR) — see the no-id comment in
    // Overview.tsx for why PASS is omitted.
    reconcileGridHead: {
      when: 'WHEN',
      result: 'RESULT',
      dur: 'DUR',
    },
    downloadThroughput: 'DOWNLOAD',
    uploadThroughput: 'UPLOAD',
    peak: 'PEAK',
    noDownloadThroughputData: 'No download throughput data yet',
    noUploadThroughputData: 'No upload throughput data yet',
    downloadThroughputAriaLabel: (peak: string) =>
      `Peak download throughput ${peak} over the recent samples`,
    uploadThroughputAriaLabel: (peak: string) =>
      `Peak upload throughput ${peak} over the recent samples`,
    reconcileMatched: (n: number) => `${n} matched`,
    reconcileNoMatch: 'no match',
  },
  peers: {
    empty: 'No peers recorded yet.',
    noArtistHistory: 'No artist-specific history.',
    artistLine: (id: number, score: string, ok: number, fail: number) =>
      `Artist #${id} — score ${score}, ${ok} succeeded, ${fail} failed`,
    // The TUI peers grid's column headers, short forms in the same idiom as
    // jobs.gridHead (#198) — SCORE/OK/FAIL rather than the generic Title Case
    // labels in `columns`, which predate this reskin.
    gridHead: {
      peer: 'PEER',
      score: 'SCORE',
      ok: 'OK',
      fail: 'FAIL',
      lastSeen: 'LAST SEEN',
    },
  },
  health: {
    neverRun: 'Never run',
    consecutiveFailures: (n: number) => `${n} consecutive failures`,
    empty: 'No modules reported.',
    // TUI Health page (#198): short state word on a dependency card, colored
    // by ModuleStatus.ready.
    ready: 'OK',
    notReady: 'ERROR',
    reconcileRateHeading: 'RECONCILE RATE',
    reconcileRateMeta: 'matched / pass · 20',
    completedHeading: 'COMPLETED',
    completedMeta: 'cumulative · 24h',
    // The mock's METRICS section names six slskdarr_* Prometheus metrics, but
    // only four exist in internal/observ and only two of those (reconcile_total,
    // album_releases_errors_total) are Prometheus-only with no JSON equivalent.
    // Rather than invent metric names for a row, these are human-readable
    // counters sourced from the same JSON the rest of this page already reads
    // (useStatus/useUploads/useShares) — the real Prometheus surface is linked
    // via metricsMeta instead of being named row by row.
    metricsHeading: 'METRICS',
    metricsMeta: 'full set at /metrics',
    metricActive: 'active downloads',
    metricQueued: 'queued',
    metricStalled: 'stalled',
    metricParked: 'parked transfers',
    metricUploads: 'active uploads',
    metricShared: 'shared files',
  },
  shares: {
    disabledNotice: 'Native Soulseek sharing is not enabled in the configuration.',
    emptyTitle: 'No shared folders configured',
    emptyBodyPrefix:
      'Sharing is the entry ticket to Soulseek. Without shares you are treated as a leech, and peers may throttle or block your downloads. Add at least one folder in',
    emptyBodySuffix:
      ', then run a rescan. If the configuration file is mounted read-only, add it there directly instead:',
    // Both keys are mandatory: internal/config rejects a share whose name is
    // blank, so a snippet without it would make the container fail to start -
    // for a user in exactly the state this warning card is shown to. Shown as
    // the fallback for a read-only config mount; Settings is the primary path.
    emptyConfigSnippet: '[[soulseek.shared_folders]]\nname = "Library"\npath = "/music/library"',
    statNever: 'Never',
    panelTitle: 'SHARED FOLDERS',
    summary: (folders: number, files: number, size: string) => `${folders} folders · ${files} files · ${size}`,
    // Report-level (SharesReport.indexedAt), not per-folder — see the
    // STALE_INDEX_MS comment in Shares.tsx for why this lives in the header
    // summary rather than as a folder-grid column.
    indexedAt: (label: string) => `indexed ${label}`,
    gridHead: {
      path: 'PATH',
      files: 'FILES',
      size: 'SIZE',
    },
    // The word next to the header's spinner while a scan is running.
    // SharesReport carries only `scanning: boolean`, no progress figure, so
    // there is deliberately no percentage or tick bar here (spec, Shares
    // section) — unlike the mock, which fakes both.
    indexing: 'indexing',
    empty: 'Nothing is being shared.',
    rescan: 'Rescan',
    rescanStarted: 'rescan started',
    rescanConflict: 'A share scan is already in progress.',
    rescanUnavailable: 'Soulseek sharing is not enabled, so a rescan cannot be started.',
    rescanFailed: 'Could not start the share rescan. Please try again.',
  },
  uploads: {
    panelTitle: 'ACTIVE UPLOADS',
    empty: 'No active uploads. Peers download from you when they queue your shared files.',
    slotsInUse: (active: number, slots: number) => `${active} / ${slots} slots`,
    // Split rather than interpolated so the peer nick can carry the mono
    // treatment the design gives it; the nick is rendered as its own span.
    toPeerPrefix: 'to',
    queuePlace: (n: number) => `queue #${n}`,
    truncated: (n: number) => `${n} more queued upload${n === 1 ? '' : 's'} not shown.`,
  },
  placeholder: {
    searchTitle: 'SEARCH',
    searchBody:
      'Manual Soulseek search is not built yet. When it lands, results will group per peer and folder, and anything downloaded can be matched and imported into Lidarr.',
    searchIssue: 'Tracked as issue #58.',
  },
  chat: {
    railHeading: 'PEERS',
    disabledNotice: 'Native Soulseek messaging is not enabled in the configuration.',
    empty: 'No conversations yet.',
    threadEmpty: 'No messages yet.',
    online: CHAT_ONLINE,
    offline: CHAT_OFFLINE,
    // Announced on each rail row so presence and the unread count reach a
    // screen reader even though they render as a color-coded dot and a bare
    // digit respectively. Unknown presence is deliberately omitted.
    threadLabel: (username: string, unread: number, online?: boolean) => {
      const presence = online === undefined
        ? ''
        : `, ${online ? CHAT_STATUS_ONLINE : CHAT_STATUS_OFFLINE}`;
      const unreadLabel = unread > 0 ? `, ${unread} unread` : '';
      return `${username}${presence}${unreadLabel}`;
    },
    you: '<you>',
    peer: (username: string) => `<${username}>`,
    // Day-divider labels (issue #247). Only the two nameable days get words;
    // anything older is shown as its sv-SE date, which needs no string. The
    // dividing rules themselves are decoration and live in Chat.tsx, the same
    // split EmptyState's doc comment describes.
    today: 'today',
    yesterday: 'yesterday',
    loadOlder: 'Load older messages',
    newConversationUsernameLabel: 'Peer username',
    newConversationUsernamePlaceholder: 'username',
    newConversationMessageLabel: 'First message',
    newConversationMessagePlaceholder: 'first message',
    newConversationSubmit: 'START',
    composerLabel: 'Message',
    composerPlaceholder: 'message',
    // The mock's "[⏎] SEND" glyph is decoration — see EmptyState's doc
    // comment on the same principle — so it is rendered as its own
    // aria-hidden span in Chat.tsx's Composer, and this string carries only
    // the accessible label.
    send: 'SEND',
    tooLong: 'Message is too long.',
    // Learned only by trying — see the doc comment atop Chat.tsx for why
    // there is no way to know this in advance.
    sendDisabled: 'Sending private messages is not enabled in the configuration.',
    sendFailed: 'Could not send the message. Please try again.',
    sendRejected: 'Message was rejected by the server.',
  },
  setup: {
    title: 'GUIDED SETUP',
    // The mock says slskdarr never writes the config file. That stopped being
    // true with issue #134 — the Config view writes it. This copy points there
    // instead of describing a workflow we no longer have.
    intro:
      'Check that each dependency answers before letting the pipeline run. Anything that fails can be corrected in the Config view, or in the configuration file directly.',
    stepSoulseek: 'SOULSEEK LOGIN',
    stepLidarr: 'LIDARR CONNECTION',
    stepShares: 'SHARED FOLDERS',
    test: 'TEST',
    testing: 'TESTING',
    stateOk: 'OK',
    stateFailed: 'FAILED',
    stateUntested: 'UNTESTED',
    stateDisabled: 'NOT ENABLED',
    fieldUrl: 'url',
    fieldApiKey: 'api key',
    fieldUsername: 'username',
    fieldPassword: 'password',
    fieldFolders: 'folders',
    fieldIndex: 'index',
    secretSet: 'configured',
    secretUnset: 'not set',
    foldersCount: (n: number) => `${n} configured`,
    indexCount: (n: number) => `${n} files`,
    sharesNoTest:
      'There is no connection test for shares. The state is derived from whether the index has found any files.',
  },
  settings: {
    notWritableNotice:
      'The configuration file is mounted read-only, so settings cannot be edited here. Mount its directory writable (e.g. ./config:/config) instead of a single-file or read-only mount to enable editing.',
    helpButtonLabel: 'Help',
    advanced: 'Advanced',
    changedBadge: 'Unsaved changes',
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
    allowPrivatePeerAddresses: 'Allow private peer addresses',
    allowPrivatePeerAddressesBlocked: 'Blocked (default)',
    allowPrivatePeerAddressesAllowed: 'Allowed',
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
      allowPrivatePeerAddresses: 'allow_private_peer_addresses',
      gluetunControlUrl: 'control_url',
      gluetunApiKey: 'api_key',
      sharedFolders: 'shared_folders',
      logLevel: 'log_level',
      dsn: 'dsn',
      observListenAddr: 'listen_addr',
      authToken: 'auth_token',
      slskdCompleteDir: 'slskd_complete_dir',
    },
    // One help text per field descriptor (see FieldDescriptor.help in
    // Settings.tsx) plus two subsection-level entries (sharedFolders,
    // gluetun) shown next to their h3 headings instead of per-input. Keys
    // mirror configKeys above, including its disambiguating prefixes
    // (lidarrApiKey vs slskdApiKey, soulseekListenAddr vs observListenAddr).
    help: {
      lidarrUrl:
        'Base URL of the Lidarr instance slskdarr syncs the wanted list from and imports finished albums into.',
      lidarrApiKey: 'Lidarr API key (Settings → General in Lidarr).',
      slskdUrl:
        'Base URL of the slskd daemon used for searching and downloading when the backend is "slskd".',
      slskdApiKey: "API key configured in slskd's own configuration (slskd.yml).",
      backend:
        'Which peer backend drives the pipeline: slskd (default) or the native Soulseek client. Switching to Soulseek requires a configured Soulseek section below; the native backend is experimental.',
      maxCandidatesPerAlbum:
        "How many ranked candidates (each one user's copy of the album) are kept per album search; the pipeline works through them best score first.",
      maxActive:
        'Global cap on jobs simultaneously in the downloading and importing phases, regardless of how many are eligible per tick. Selecting jobs hold no slot and wait until one frees.',
      maxRetries:
        'How many failed search cycles (a search with no viable results, or a candidate cache fully tried and failed) a job may accumulate before it is marked failed. Individual candidate failures within a cycle do not count.',
      maxInflightPerPeer:
        'Max files of a candidate album handed to its peer at once (counted per candidate, so two jobs downloading from the same peer each get their own budget); remaining files are released as earlier ones finish. Keeping this low avoids tripping the per-user queued-megabyte limit peers enforce.',
      maxTransferRetries:
        'Per-file retry budget: how many times a transiently failed transfer (a peer queue-limit rejection, a stall, or a transfer lost to an slskd restart) is re-queued before it is errored. Permanent rejections (file not shared, banned) are never retried; 0 disables retries.',
      minBitrate:
        'Quality floor applied per file during matching: lossy files below this bitrate (kbps) are dropped before ranking, while lossless files (FLAC) always pass regardless of reported bitrate.',
      transferDeadline:
        "Maximum time a single file transfer may take from enqueue — including time spent waiting in the peer's remote queue — before it is cancelled as overdue, which fails the candidate so the next one is tried.",
      stallTimeout:
        'How long a started transfer may go without byte progress before it is treated as stalled, cancelled, and retried within the transfer-retry budget.',
      searchTimeout:
        'Upper bound on how long a single search waits for peer responses. The native backend always waits the full duration; the slskd backend collects as soon as slskd reports the search complete, and retries an empty search a few times.',
      backoffBase:
        'Base of the exponential backoff applied when a search cycle leaves a job with nothing to try; the first wait is twice this value and doubles with each further failure.',
      backoffCap: 'Upper bound on exponential backoff growth.',
      candidateTtl:
        "How long a cached search result stays trustworthy. When a candidate older than this is about to be activated, the job's whole candidate cache is discarded and a fresh search is made instead.",
      failedReviveAfter: 'How long a permanently failed job waits before being revived for another attempt.',
      stuckAfter:
        'How long an importing job may keep failing its Lidarr verification without a state change before the candidate is failed and the next one is tried.',
      tickTimeout: 'Upper bound on the total execution time of a single pipeline tick.',
      importConfirmTimeout:
        'How long an import may sit unconfirmed (Lidarr ManualImport command runs asynchronously) before it is treated as failed and rotated to the next candidate.',
      wantedSyncInterval: 'How often the wanted list is refreshed from Lidarr.',
      discoveryInterval: 'How often the discovery phase runs.',
      selectingInterval: 'How often the selecting phase runs.',
      downloadingInterval: 'How often the downloading phase runs.',
      importingInterval: 'How often the importing phase runs.',
      manualImportTimeout:
        "Upper bound on Lidarr's manual-import folder scan, which parses audio tags per file and can run far longer than a normal API call on large folders.",
      importRetryCooldown:
        'How long the importing phase waits before re-attempting a failed manual-import scan on the same job, so a slow-scanning folder is not hammered every importing interval.',
      weightFormat: 'Relative weight of preferred audio format (e.g. FLAC over MP3) when ranking candidates.',
      weightBitrate: 'Relative weight of higher bitrate when ranking candidates.',
      weightReliability:
        "Relative weight of the peer's current upload availability (a free upload slot and an empty queue) when ranking candidates.",
      weightFileCount:
        "Relative weight of the number of files in a release when ranking candidates; more files score higher. Releases outside the album's known track-count range are filtered out separately.",
      weightKnownUser:
        "Relative weight of the peer's decayed known-good/known-bad history when ranking candidates; the history factor is normalized to 0..1 and applied once per candidate.",
      serverAddress: 'Host:port of the central Soulseek server (default server.slsknet.org:2242).',
      username: 'Username for the Soulseek account the native client logs in with.',
      password: 'Password for the Soulseek account the native client logs in with.',
      soulseekListenAddr:
        'Address slskdarr listens on for incoming peer connections; its port is advertised to the server after login, so peers must be able to reach it directly. With Docker, the published host port must equal the container port, or use host networking instead.',
      allowPrivatePeerAddresses:
        'Allow direct peer connections to private, loopback, and link-local addresses advertised by the Soulseek server. Keep this blocked unless trusted peers are intentionally reachable on those networks.',
      uploadSlots:
        'Number of upload slots; negotiation and streaming both occupy a slot, and additional requests wait in a bounded global queue.',
      gluetunControlUrl: 'URL of the gluetun control server, used at startup to fetch the forwarded port.',
      gluetunApiKey: 'API key for the gluetun control server, if its auth is enabled.',
      sharedFolders:
        'Shares use explicit public names so local host/container paths never appear in browse or search results. Mount each path read-only. Shares are scanned at startup (a settings save restarts the process and rescans); SIGHUP triggers a rescan without a restart.',
      gluetun:
        'Run behind gluetun for VPN port forwarding: at startup slskdarr asks the gluetun control server for the currently forwarded port and listens on it instead. The port part of the listen address above is ignored (its host is still used).',
      logLevel:
        'Minimum log level: debug, info (default), warn, or error. Use debug to trace peer and transfer negotiation in the native Soulseek client (verbose).',
      dsn: 'PostgreSQL connection string. The schema is created automatically on startup; the database and user must already exist.',
      observListenAddr:
        'Address and port the web UI and API listen on. A non-loopback address requires auth_token to be set; after changing it the UI moves to the new address.',
      authToken:
        'Token required for the dashboard, JSON API, /status, and /metrics whenever the listener is not loopback-only. Generate one with openssl rand -hex 32, keep it out of URLs and logs, and terminate TLS at a trusted reverse proxy before exposing the listener beyond a private network.',
      slskdCompleteDir:
        "Directory where finished downloads land, as seen by both slskdarr and Lidarr. With the slskd backend it must also point at the same location as slskd's completed-downloads directory, so all containers must mount it consistently.",
    },
    connections: 'CONNECTIONS',
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
