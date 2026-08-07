// Every user-facing string lives here. Components must never inline text.
// This is i18n preparation, not i18n — see #86.
const CHAT_ONLINE = 'ONLINE';
const CHAT_OFFLINE = 'OFFLINE';
const CHAT_STATUS_ONLINE = 'online';
const CHAT_STATUS_OFFLINE = 'offline';

export const t = {
  app: {
    name: 'slusk',
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
      subtitle: 'Every album slusk is tracking, from queued to imported',
    },
    search: {
      title: 'Search',
      // Deliberately does not promise Lidarr import: a manual download only
      // imports when the user identified it (via IdentifyModal, issue #321)
      // and Lidarr already has that release group in its library. Otherwise
      // it downloads and stops at the terminal NOT_IMPORTED state (#59) — see
      // app.Jobs.Create's doc comment. This subtitle stays conditionality-free
      // rather than spelling that branch out inline.
      subtitle: 'Query the Soulseek network directly and download what you find',
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
      subtitle: 'slusk validates the config you already wrote — it never writes it',
    },
    settings: {
      title: 'Config',
      subtitle: 'Resolved runtime configuration, as slusk sees it',
    },
    // Not in the mock (docs/design/slusk-tui.dc.html drops both views),
    // but Events and Peers are still shipped — see Page.tsx's doc comment.
    // Titles/subtitles are invented here in the same voice as the rest of
    // this object rather than left unstyled.
    events: {
      title: 'Events',
      subtitle: 'The raw job-event log across every job, newest first',
    },
    peers: {
      title: 'Peers',
      subtitle: 'Everyone slusk has downloaded from, ranked by reliability',
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
    // AGPL § 13 requires a network-interacting version to "prominently offer"
    // its Corresponding Source (issue #391, deriving from #380). The visible
    // label is a bare word to sit inside the brand cell's terminal idiom; the
    // accessible name spells out what it offers, because "SOURCE" alone read
    // out of context could equally mean a download source or a search source.
    // The visible text is contained in the accessible name, so WCAG 2.5.3
    // (Label in Name) still holds for anyone driving this by voice.
    sourceLabel: 'SOURCE',
    sourceLabelAccessible: 'Source code for this version',
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
    // Wanted/selecting/waiting (issue #416) split what used to be grouped
    // under queued/active — see JobStatus's doc comment in api/types.ts.
    wanted: 'Wanted',
    selecting: 'Selecting',
    queued: 'Queued',
    waiting: 'Waiting',
    active: 'Active',
    stalled: 'Stalled',
    importing: 'Importing',
    done: 'Done',
    failed: 'Failed',
    parked: 'Parked',
    // A manual job (issue #59) that finished downloading with no Lidarr album
    // to import into. Not a failure: the files are on disk.
    notImported: 'Not imported',
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
    NI: 'NI',
    WA: 'WA',
    SE: 'SE',
    WT: 'WT',
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
    WA: 'Wanted',
    SE: 'Selecting candidate',
    WT: 'Waiting for next file',
    // Issue #59. Two different routes end here — the download was never
    // identified against a release group, or it was and that release group
    // is not in Lidarr's library — and the tag cannot tell them apart, so it
    // states the outcome's proximate cause rather than guessing which one
    // applied. The specific reason is recorded as a job event and shown in
    // the job's detail view, which is where someone troubleshooting looks.
    NI: 'Downloaded, not imported — no Lidarr album to import into',
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
    // Terminal state for a manual job (issue #59) that finished downloading
    // with no Lidarr album to import into. See tagTitle.NI's comment on why
    // the wording states the outcome rather than which of the two routes
    // there produced it.
    NOT_IMPORTED: 'Not imported',
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
    search_excluded: 'Search excluded by server',
    candidate_selected: 'Candidate selected',
    candidate_rejected: 'Candidate rejected',
    attempt_failed: 'Attempt failed',
    attempt_succeeded: 'Attempt succeeded',
    transfer_stalled: 'Transfer stalled',
    import_ok: 'Import completed',
    import_rejected: 'Import rejected',
    job_failed: 'Job failed',
    quarantined: 'Files quarantined',
    // Written from two different sites in the backend (issue #59) — the
    // download was never identified against a release group, or it was and
    // that release group isn't in Lidarr's library. Deliberately names only
    // the outcome; the distinct detail text carried in the event's own
    // `detail` field (rendered alongside this label) is where the specific
    // cause lives.
    not_imported: 'Downloaded, not imported',
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
  // The shared prev/numbered/next control (see components/tui/Pager) — text
  // that names no domain, since any list view can mount it. Lives here rather
  // than under the route that used to own it (#425): a string called
  // `jobs.nextPage` on a control the Peers view also mounts would be a name
  // that lies about its scope.
  pager: {
    previousPage: '[,] PREV',
    nextPage: 'NEXT [.]',
    pageLabel: (page: number) => `Page ${page}`,
  },
  jobs: {
    // Short because the field is the flexible element on a full filter row and
    // shrinks to 120px there (see .filterBox in Jobs.module.css). The longer
    // 'Search artist, album, peer…' clipped mid-word, which reads as breakage
    // rather than as a hint. What is searchable is legible from the column
    // headers beside it.
    searchPlaceholder: 'Search…',
    noMatch: 'No jobs match the filter.',
    back: '← Back',
    cancel: 'Cancel',
    retry: 'Retry',
    parkedExplanation:
      'Repeated backend disappearance exhausted transfer retries, so automation stopped. Retry discards prior candidates and transfers, then starts a fresh search and download cycle.',
    // 'Force search' used to double as both "re-run the automated pipeline"
    // and (implicitly) "search manually" — issue #376 split those into two
    // distinct actions, so this one is named for what it actually does:
    // Store.ForceSearchJob discards prior candidates and transfers, resets
    // retries/empty_searches/not_before, and puts the job back through
    // Discovery from scratch — not a resend of the same candidates.
    forceSearch: 'Re-run pipeline',
    // Navigates to the search page pre-filled with the job's artist/album
    // (issue #376) — a manual, user-driven Soulseek search, distinct from
    // forceSearch above which re-runs the automated pipeline.
    manualSearch: 'Manual search',
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
    forceSearchFailed: 'Could not re-run the pipeline. The job may already be active.',
    deleteFailed: 'Could not delete the job. It may currently be importing.',
    sleepingUntil: (time: string) => `Sleeping until ${time}`,
    candidates: (tried: number, max: number) => `${tried} of ${max} candidates tried`,
    nextAttempt: (time: string) => `Next attempt: ${time}`,
    retries: (n: number) => `${n} retries`,
    queuePosition: (n: number) => `queue #${n}`,
    // The MusicBrainz release-group MBID the job was identified against
    // (issue #59), shown in the job detail view — it's the identity the
    // import step resolves against, so it's real troubleshooting scent for a
    // manual job stuck at NOT_IMPORTED. Omitted entirely when the job has
    // none.
    albumMbid: (mbid: string) => `MusicBrainz release group: ${mbid}`,
    // The attempt header's file count (job detail page) and a transfer's own
    // retry count — both were inline template strings before this reskin.
    fileCount: (n: number) => `${n} files`,
    transferRetries: (n: number) => `${n} retries`,
    verifying: 'verifying',
    showDetails: 'Show details',
    hideDetails: 'Hide details',
    moreFiles: (n: number) => `+${n} more files`,
    // FILES is the only titled column in the TUI expansion (mock,
    // docs/design/slusk-tui.dc.html:190) — the meta column on the left has
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
      waiting: 'WAITING',
      selecting: 'SELECTING',
      wanted: 'WANTED',
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
    // paginationLabel stays: it names *these* pages ("Job pages") and is the
    // one pager-adjacent string that is genuinely route-specific. The prev/
    // next/page-N labels moved to `t.pager` with the component (#425).
    paginationLabel: 'Job pages',
    resultRange: (start: number, end: number, total: number) => `${start}–${end} of ${total} jobs`,
    // Compact peer-queue position for the dense PROGRESS cell, where "queue
    // #4" (queuePosition above) would overflow the column.
    queueShort: (n: number) => `P${n}`,
    // Flash confirmations (see FlashContext) for the three row actions —
    // mutations the row itself won't visibly reflect before the next poll.
    // Bulk retry (issue #378) — the whole filtered set, never the page on
    // screen, which is why every string here talks about the filter rather
    // than about rows.
    bulkRetry: {
      button: 'Retry all',
      dialogLabel: 'Confirm bulk retry',
      dialogTitle: 'RETRY ALL',
      confirm: 'Retry them',
      cancel: 'Cancel',
      // The parked status is exactly state IN ('PARKED','ORPHANED'), so its
      // facet count is the retryable set and can be stated plainly.
      parkedBody: (n: number) =>
        `Retry all ${n} parked jobs matching the current filter — not just this page.`,
      // The failed status is derived, not a state: it also matches a job the
      // pipeline is still working through (its current candidate's transfers
      // have all failed, and the next candidate has not been tried yet), which
      // the server will refuse to touch. The count is therefore an upper
      // bound, and the copy has to say so — the response carries the truth.
      failedBody: (n: number) =>
        `Retry the failed jobs matching the current filter — not just this page. Up to ${n}: some of those are still mid-retry in the pipeline and will be left alone.`,
      lidarrNote:
        'Lidarr-sourced jobs start a fresh search; manually created jobs retry the same peer.',
      // Reported as what happened, not as success/failure.
      resultFlash: (retried: number, skipped: number) =>
        `${retried} retried, ${skipped} skipped`,
      failed: 'Could not run the bulk retry.',
    },
    retryFlash: (id: number) => `retried #${id}`,
    cancelFlash: (id: number) => `cancelled #${id}`,
    forceSearchFlash: (id: number) => `pipeline re-run queued for #${id}`,
    deleteFlash: (id: number) => `deleted #${id}`,
  },
  events: {
    filterPlaceholder: 'Filter events…',
    empty: 'No events.',
  },
  overview: {
    empty: 'Nothing in flight.',
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
    inFlightCountMeta: (n: number) => (n === 1 ? '1 album' : `${n} albums`),
    // Shown instead of inFlightCountMeta when the panel cannot fit every
    // in-flight job: max_active can exceed the panel's row count, and silently
    // dropping the remainder would read as "this is all of it".
    //
    // Says "albums", not "in flight": the IN FLIGHT stat cell above counts
    // active file transfers (/status's active field is len(ActiveTransfers)),
    // not jobs, so two numbers labelled the same way would sit two rows apart
    // meaning different things. The panel lists albums; it says so.
    inFlightTruncatedMeta: (shown: number, total: number) => `${shown} of ${total} albums`,
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
    // Not "downloading N peers": status.active is len(ActiveTransfers), which
    // spans queued/in_progress/stalled transfers (not all "downloading"), and
    // it's a transfer count, not a distinct-peer count.
    subInFlight: (transfers: number) =>
      transfers === 1 ? '1 active transfer' : `${transfers} active transfers`,
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
    // The RECENTLY FINISHED panel (#287): jobs that reached DONE or FAILED
    // within the backend's window (store.DashboardFinishedWindow).
    finishedHeading: 'RECENTLY FINISHED',
    // Deliberately says nothing about how long "recently" is: the window is a
    // Go constant (store.DashboardFinishedWindow) and this is a TypeScript
    // string, so no test in either suite could catch them contradicting each
    // other. An agnostic phrasing cannot go out of date.
    noneFinished: 'Nothing finished recently',
    finishedGridHead: {
      status: 'ST',
      album: 'ALBUM',
      peer: 'PEER',
      when: 'WHEN',
    },
    // The FAILED panel (#310): jobs whose STATE is FAILED, time-unbounded —
    // unlike RECENTLY FINISHED above, there is no window, since a failure a
    // caller hasn't dealt with yet is still worth surfacing whenever it
    // happened. "FAILED JOBS", not "FAILED IMPORTS": a row here can equally
    // be a search or download failure, not only an import rejection.
    failedHeading: 'FAILED JOBS',
    noneFailed: 'Nothing failed',
    failedGridHead: {
      status: 'ST',
      album: 'ALBUM',
      reason: 'REASON',
      when: 'WHEN',
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
    // Reachable when the list shrinks under a page the user is already on.
    // Deliberately not `empty`: there are peers, just none on this page, and
    // the two claims are not interchangeable.
    pastTheEnd: 'No peers on this page.',
    noArtistHistory: 'No artist-specific history.',
    // The expansion is a network call of its own since #424, so it has the
    // same two failure modes every other GET in this app has. Worded for the
    // one row it applies to rather than reusing t.query.*, which reads as a
    // statement about the whole page.
    artistHistoryLoading: 'Loading artist history…',
    artistHistoryFailed: 'Could not load this peer’s artist history.',
    // An artist with no known name is labelled by its Lidarr id. That id is
    // useless to a user who only knows Lidarr and Soulseek — which is exactly
    // why it is the fallback and not the default — but it is true, and a
    // placeholder name would not be. See interface-must-not-invent-data.
    artistLabel: (id: number, name: string) => (name === '' ? `Artist #${id}` : name),
    artistLine: (label: string, score: string, ok: number, fail: number) =>
      `${label} — score ${score}, ${ok} succeeded, ${fail} failed`,
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
    // Names *these* pages ("Peer pages") rather than reusing jobs.paginationLabel,
    // so a screen reader landing on the control knows which list it moves.
    paginationLabel: 'Peer pages',
    resultRange: (start: number, end: number, total: number) => `${start}–${end} of ${total} peers`,
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
    // The mock's METRICS section names six slusk_* Prometheus metrics, but
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
    // The permanent scan failure (SharesReport.lastError, issue #408). It
    // takes the place of emptyTitle above rather than stacking with it: a
    // failed scan also reports zero folders, and "no shared folders
    // configured" would then be a confident wrong diagnosis of a share that
    // is configured perfectly well. No cause is named here — the backend's
    // message is rendered verbatim below the body, so this copy stays true
    // whatever gets classified as permanent later.
    scanFailedTitle: 'Share index could not be published',
    scanFailedBody:
      'The last scan failed in a way that retrying will not fix, so no files are being shared and slusk has stopped trying. It reported:',
    scanFailedSuffix: 'Resolve the cause, then run a rescan.',
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
    historyTitle: 'UPLOAD HISTORY',
    historyEmpty: 'No finished uploads yet. Completed, aborted and rejected transfers are recorded here.',
    // Words rather than the two-letter t.tag codes: this list has the width,
    // and a code needs a glossary the user does not have.
    historyStatus: {
      completed: 'COMPLETED',
      aborted: 'ABORTED',
      rejected: 'REJECTED',
    } as const,
    historyLoadOlder: 'Load older',
    // The caret's aria-label pair (issue #371) — same naming as
    // t.jobs.showDetails/hideDetails, kept as its own pair rather than reused
    // directly since this is a different expansion with its own labels below.
    historyShowDetails: 'Show details',
    historyHideDetails: 'Hide details',
    // Expansion labels (issue #371): size and finished-at move off the row
    // and behind the caret, alongside the existing detail line.
    //
    // historySizeLabel says "transferred", not "size" (issue #366/#371
    // review, FIX 6): the value behind it is `transferred(entry)`, which on
    // an aborted row is sent-of-total, not the file's size — "size" reads as
    // a claim about the whole file even though the row visibly stopped
    // partway through. Matches JobExpansion's own transferredLabel for the
    // same field on the download side.
    historySizeLabel: 'transferred',
    historyFinishedLabel: 'finished',
  },
  // Manual Soulseek search (issue #58). See docs/design/
  // slusk-dashboard.dc.html lines 336-520 for the visual spec this
  // follows — translated to English (that mock is Swedish; strings.ts is
  // English throughout, unlike every other route in this file) and adapted
  // onto the TUI restyle's component vocabulary (Panel/Chip/QueryNotice/
  // EmptyState) rather than that mock's own markup.
  search: {
    queryPlaceholder: 'Search Soulseek — artist, album, track…',
    // Visually hidden, but a real <label htmlFor> rather than relying on the
    // placeholder: a placeholder-derived accessible name (HTML-AAM's last
    // resort) disappears the moment the user types, which is exactly when a
    // screen-reader user re-reads the field. WCAG 3.3.2.
    queryLabel: 'Search query',
    submit: 'Search',
    idleTitle: 'Search Soulseek directly',
    idleBody:
      'Results are grouped per peer and folder. Anything you download here can be enqueued the same way an automatic job is.',
    // Example query chips shown only in the idle state.
    examples: ['Radiohead In Rainbows', 'Kind of Blue', 'Nirvana Nevermind'],
    noHitsTitle: (query: string) => `No hits for "${query}"`,
    noHitsBody: 'No peer on the network is sharing this right now. Try a different spelling, drop the year, or search on the artist alone.',
    newSearch: 'New search',
    // The results header's live counter. Composed as
    // `${count}${suffix}` — see Search.tsx — rather than one template
    // string, since streaming/complete/truncated are independent flags,
    // not a fixed set of combinations.
    resultsCount: (n: number) => `${n} results`,
    streamingSuffix: 'streaming in…',
    completeSuffix: 'search complete',
    // Distinct from completeSuffix: the session was evicted server-side
    // before it finished (see replaceSearchGroups' `expired` branch), so
    // what is on screen is whatever had already arrived, not a finished
    // search. "Search complete" here would be a lie.
    expiredSuffix: 'session expired — results may be incomplete',
    truncatedSuffix: 'showing first 2000',
    askingPeers: 'Asking peers on the network…',
    sortLabel: 'Sort',
    sortOptions: {
      best: 'Best match',
      size: 'Size',
      speed: 'Speed',
      avail: 'Availability',
    },
    // Format chip row, derived from the distinct formats present in the
    // current results (issue #129 lifts this derivation rather than
    // forking it — see the exported helper in Search.tsx). An empty active
    // set already means "all formats", so the row needs no explicit
    // all-formats chip and therefore no string for one.
    noFormatMatch: 'No results match the selected format filters.',
    bestMatch: 'BEST MATCH',
    freeSlot: 'Free slot',
    // A peer with no free slot and nobody waiting: `queuePosition(0)` read
    // as "Queue: 0", which looks like the good case next to freeSlot even
    // though it is the opposite one. Both facts are true; only this phrasing
    // makes them stop contradicting each other.
    noFreeSlot: 'No free slot',
    queuePosition: (n: number) => `Queue: ${n}`,
    peerAndSpeed: (peer: string, speed: string) => `${peer} · ${speed}`,
    trackCount: (n: number) => `${n} track${n === 1 ? '' : 's'}`,
    // `trackCount` counts audio only, while the download buttons enqueue
    // every file in the folder (covers, .nfo, .m3u) — so a card could read
    // "10 tracks" beside "Download selected (13)" with nothing accounting
    // for the other three. Rendered only when there is a difference.
    extraFiles: (n: number) => `+${n} extra file${n === 1 ? '' : 's'}`,
    // `parent` is the peer's parent DIRECTORY name (path.Base(path.Dir(folder))),
    // never a resolved artist — a peer sharing /Music/Various Artists/In Rainbows/
    // yields "Various Artists". Rendered with a trailing slash in the mono face
    // so it reads as a path segment rather than the "Album — Artist" idiom, plus
    // this hidden prefix and title for anyone who can't see that treatment.
    folderLabel: 'Peer folder:',
    folderTitle: (parent: string) => `Parent folder on the peer: ${parent}/ — not a resolved artist`,
    downloadAlbum: 'Download album',
    downloadSelected: (n: number) => `Download selected (${n})`,
    queuedNotice: 'Queued for download.',
    // Client-owned friendly text rather than the server's raw sentinel
    // message (app.ErrRemoteFileBusy) — matches the rest of this file's
    // convention of translating failure sentinels into UI copy (e.g.
    // jobs.cancelFailed) rather than surfacing backend error strings.
    busyNotice: "This peer's files are already claimed by another download in progress.",
    downloadFailed: 'Could not start the download. Try again.',
    startFailed: 'Search failed to start. Try again.',
    busyRetry: 'Too many searches are already running — try again shortly.',
    // The Identify & download modal (issue #321). Soulseek carries no
    // metadata — a search result is only a peer's folder name — so this
    // flow lets the user confirm the canonical MusicBrainz artist/album
    // before the job is created, rather than posting the folder guess
    // (group.parent/group.title) as if it were fact.
    identify: {
      // Trigger button in SearchResultCard's actions row. Deliberately does
      // not say "import" — the app can enqueue a download but can never
      // promise Lidarr will pick it up (see lidarrUnknown and the two
      // *Missing lines below).
      button: 'Identify & download',
      identified: '✓ Identified',
      // The dialog's own header label — deliberately its own entry rather
      // than reusing `button`.toUpperCase(): the mock treats the trigger
      // ("[m] IDENTIFY") and the panel title ("IDENTIFY & DOWNLOAD") as
      // different strings, and reusing `button` here would silently retitle
      // the dialog if the button's own copy ever changed. Rendered via
      // .headerLabel, which — unlike .fieldLabel below — has no
      // text-transform, so this one is typed in caps directly, matching the
      // mock's own literal text.
      dialogTitle: 'IDENTIFY & DOWNLOAD',
      dialogLabel: 'Identify and download',
      close: 'Close',
      // Sentence case in source, all rendered uppercase via .fieldLabel's
      // text-transform (see IdentifyModal.module.css) — kept sentence case
      // here rather than typed in caps because these strings also serve as
      // the two inputs' accessible names, and a screen reader should not
      // read a label as if it were being shouted.
      identifyingFolder: 'Identifying folder',
      artistLabel: 'Artist (guessed — edit if wrong)',
      albumLabel: 'Album (guessed — edit if wrong)',
      searchButton: 'Search MusicBrainz',
      // The endpoint 422s on a blank album, so the button is disabled rather
      // than silently doing nothing when clicked (review item H) — this is
      // its disabled-state reason, exposed via title/aria-describedby.
      albumRequired: 'Enter an album to search.',
      searching: 'searching musicbrainz…',
      // Suggestions table column headers, matching the mock exactly —
      // EDITIONS is a genuine single-call field on the combined search
      // result (MusicBrainzSearchResult.editionCount), not a per-row lookup.
      colArtistAlbum: 'ARTIST / ALBUM',
      colType: 'TYPE',
      colYear: 'YEAR',
      colEditions: 'EDITIONS',
      // The suggestions table's own EDITIONS cell — the mock's short form
      // ("3 eds.", "1 ed."), distinct from editionCount below (the full
      // "3 editions" used in the "WILL BE RECORDED AS" summary once a row is
      // picked). Same number, two different renderings, matching the mock's
      // own m.editions vs selEditions split.
      editionCountShort: (n: number) => `${n} ed${n === 1 ? '.' : 's.'}`,
      notIt: 'Not it? Edit the fields above and search again.',
      // The edition PICKER's truncation notice (MusicBrainzEditionListResult
      // is a genuinely paginated/capped list of one release-group's
      // editions), distinct from showingBestOf below.
      showingOf: (shown: number, total: number) => `showing ${shown} of ${total}`,
      // The SUGGESTIONS list's truncation notice. Deliberately not phrased
      // like showingOf: GET /api/identify/search is a relevance-ranked
      // search, not a paginated catalogue — `total` routinely reaches the
      // hundreds, and "showing N of 412" would wrongly imply the rest are
      // one more click away.
      showingBestOf: (shown: number) => `showing the ${shown} best match${shown === 1 ? '' : 'es'}`,
      noMatchesTitle: 'NO MATCHES',
      noMatchesBody: 'MusicBrainz returned nothing for these terms. Edit the artist/album above and search again.',
      searchAgain: 'Search again',
      unavailableTitle: 'MUSICBRAINZ UNAVAILABLE',
      unavailableBody: 'Lookup service is down or rate-limited right now. You can still download this folder without identifying it.',
      retry: 'Retry',
      willBeRecordedAs: 'Will be recorded as',
      // `n` is the SEARCH result's own editionCount (MusicBrainzSearchResult,
      // GET /api/identify/search) — a single-call field that arrives with
      // the row itself, NOT the editions picker's own total (from the
      // separate, genuinely paginated GET /api/identify/albums/{id}/editions
      // — see showingOf above). IdentifyModal.tsx's "WILL BE RECORDED AS"
      // summary uses this one; the picker's own truncation notice uses the
      // other. Mixing the two up is exactly the crossing the tests
      // (IdentifyModal.test.tsx's "reaches the selected state…" case) exist
      // to catch.
      editionCount: (n: number) => `${n} edition${n === 1 ? '' : 's'}`,
      // The edition picker (the one deliberate addition beyond the mock —
      // see the brief). The mock computes its verdict against a band across
      // every edition of the release-group, which real data makes useless
      // (an 8-97 track spread once box sets share the group with the
      // album); picking one edition first is what makes its own "matches
      // THIS edition" copy below actually true.
      editionPickerLabel: 'Edition',
      editionUnknownTracks: 'tracks unknown',
      verdictIncomplete: (have: number, want: number) => `INCOMPLETE — ${have} of ${want} tracks present`,
      verdictComplete: (n: number) => `COMPLETE — ${n} tracks matches this edition`,
      verdictMore: (have: number, want: number) =>
        `${have} TRACKS FOUND — more than this edition (${want} tracks); likely a different edition, or more than one release in the folder`,
      // Two distinct strings rather than one shared across both cases (issue
      // #321 review item B): "this edition has no track listing" asserts a
      // referent that, in the noEdition case, does not exist — no edition is
      // selected at all (an empty editions list, or a failed editions fetch;
      // see IdentifyModal's computeVerdict and pickResult's catch). Naming
      // an edition that isn't there is the same class of defect the
      // canonical-artist fallback chain was fixed for.
      verdictUnknownEdition: 'COMPLETENESS UNKNOWN — this edition has no track listing',
      verdictNoEdition: 'COMPLETENESS UNKNOWN — no edition selected',
      // The three outcomes of the single Lidarr status line (see
      // IdentifyModal's lidarrLine). #331 briefly rendered artist and album
      // status as two separate lines, which said the same thing twice — an
      // absent artist implies an absent album. These name which of the three
      // real situations holds, and each states the import consequence once.
      lidarrInLibrary: 'IN LIDARR LIBRARY — matched download will be imported',
      // Deliberately conditional, unlike the artist-and-album line below. When
      // the artist IS in Lidarr, the album row can still appear on its own:
      // Lidarr materialises a discography asynchronously, and the import step
      // resolves the album when the download finishes, not now. A user hit
      // exactly this — the modal said the album was missing and it imported
      // anyway. Promising it "won't be imported" would be inventing a
      // certainty the lookup cannot support.
      lidarrAlbumMissing: 'ALBUM NOT IN LIDARR — imports only if Lidarr adds it in time',
      // No such hedge here: with the artist absent too, nothing is going to
      // create either of them between now and the end of the download.
      lidarrArtistAndAlbumMissing: "ARTIST & ALBUM NOT IN LIDARR — download won't be imported",
      lidarrUnknown: 'LIDARR STATUS UNKNOWN — service unreachable',
      back: '‹ Back',
      confirm: 'Confirm identification',
      // Shown, and CONFIRM disabled, when MusicBrainz supplied no artist AND
      // the user has also blanked the artist field (review item A). Posting
      // `group.parent` — the peer's parent directory — here would be exactly
      // the folder-name guess #321 exists to stop sending; the user is not
      // blocked (closing the modal and using the card's own download
      // buttons is always available), just not allowed to CONFIRM a
      // fabricated identity.
      noCanonicalArtist: 'Enter an artist above — MusicBrainz did not supply one for this release.',

      // ---- Lidarr add-artist flow (issue #331) ----
      //
      // Shown below the status line when the picked search
      // result carries no artistId at all (release-group with an empty
      // artist-credit) — case 3 of the brief. Never invents an artist to
      // resolve one; see canonicalArtistOf's own comment on the same rule.
      // Confirming here still forwards the release-group MBID (confirm()'s
      // withMbid defaults to true even in this branch), so it must not claim
      // the download won't import — it will, if Lidarr already has the album.
      noArtistId: "MusicBrainz did not supply an artist ID for this release, so a new artist can't be added to Lidarr from here. If the album is already in Lidarr's library, downloading will still import it.",
      // The two paths offered when the album is confirmed NOT in the
      // library and both artist/album lookups succeeded (case 2). Neither
      // is the default action — both render as equal buttons, per the
      // brief's "download anyway is legitimate and must not be hidden".
      // Same wording as addSubmit below ("&", not "and") — this button and
      // that one name the same action (open the form vs. submit it), and two
      // spellings for one action read as sloppy rather than deliberate.
      addToLidarr: 'Add to Lidarr & download',
      downloadAnyway: 'Download anyway',
      addOptionsLoading: 'loading lidarr options…',
      addOptionsFailed: "Could not load Lidarr's root folders and profiles.",
      // Shown alongside Retry when the add-options fetch fails, so the user
      // can still get back to the two-path choice (and "download anyway")
      // instead of being stuck with no way out but closing the modal.
      addOptionsFailedBack: 'Cancel',
      rootFolderLabel: 'Root folder',
      rootFolderInaccessible: 'inaccessible',
      // Shown instead of the form when Lidarr's add-options returned no
      // accessible root folder, or no quality/metadata profile at all — the
      // add form has nothing usable to submit, so it isn't rendered pretending
      // otherwise. "Download anyway" stays reachable via Cancel below.
      noAccessibleRootFolder: 'No accessible root folder is configured in Lidarr — check its settings there.',
      noUsableProfiles: 'No quality or metadata profile is configured in Lidarr — check its settings there.',
      qualityProfileLabel: 'Quality profile',
      metadataProfileLabel: 'Metadata profile',
      addSubmit: 'Add to Lidarr & download',
      addCancel: 'Cancel',
      addSubmitting: 'adding to lidarr…',
      // Definite failure — every POST /api/lidarr/artists status except 502
      // means the add genuinely did not happen. Contrast addUncertain below.
      addArtistFailed: 'Could not add the artist to Lidarr. Try again, or download without importing.',
      // The 502 { code: "addUncertain" } case: the request may or may not
      // have created the artist server-side. Deliberately does not say
      // "failed" — that would be inventing certainty the response doesn't
      // have. A retry is safe (Lidarr's add is idempotent on the MBID), so
      // it's offered as the next step rather than only "check Lidarr".
      addUncertain:
        'Could not confirm whether the artist was added to Lidarr — it may have succeeded. Check Lidarr, or try again; retrying is safe.',
    },
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
    // The mock says slusk never writes the config file. That stopped being
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
      'slusk writes the previous configuration to config.toml.bak before saving. Restoring it requires editing the file directly and a manual restart.',
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
        'Base URL of the Lidarr instance slusk syncs the wanted list from and imports finished albums into.',
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
        'Address slusk listens on for incoming peer connections; its port is advertised to the server after login, so peers must be able to reach it directly. With Docker, the published host port must equal the container port, or use host networking instead.',
      allowPrivatePeerAddresses:
        'Allow direct peer connections to private, loopback, and link-local addresses advertised by the Soulseek server. Keep this blocked unless trusted peers are intentionally reachable on those networks.',
      uploadSlots:
        'Number of upload slots; negotiation and streaming both occupy a slot, and additional requests wait in a bounded global queue.',
      gluetunControlUrl: 'URL of the gluetun control server, used at startup to fetch the forwarded port.',
      gluetunApiKey: 'API key for the gluetun control server, if its auth is enabled.',
      sharedFolders:
        'Shares use explicit public names so local host/container paths never appear in browse or search results. Mount each path read-only. Shares are scanned at startup (a settings save restarts the process and rescans); SIGHUP triggers a rescan without a restart.',
      gluetun:
        'Run behind gluetun for VPN port forwarding: at startup slusk asks the gluetun control server for the currently forwarded port and listens on it instead. The port part of the listen address above is ignored (its host is still used).',
      logLevel:
        'Minimum log level: debug, info (default), warn, or error. Use debug to trace peer and transfer negotiation in the native Soulseek client (verbose).',
      dsn: 'PostgreSQL connection string. The schema is created automatically on startup; the database and user must already exist.',
      observListenAddr:
        'Address and port the web UI and API listen on. A non-loopback address requires auth_token to be set; after changing it the UI moves to the new address.',
      authToken:
        'Token required for the dashboard, JSON API, /status, and /metrics whenever the listener is not loopback-only. Generate one with openssl rand -hex 32, keep it out of URLs and logs, and terminate TLS at a trusted reverse proxy before exposing the listener beyond a private network.',
      slskdCompleteDir:
        "Directory where finished downloads land, as seen by both slusk and Lidarr. With the slskd backend it must also point at the same location as slskd's completed-downloads directory, so all containers must mount it consistently.",
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
  // The login/first-run card (issue #279) — see docs/design/slusk-login.dc.html
  // and Login.tsx. It renders outside <Layout> (no nav, no top bar), before
  // any account is known to exist, so it owns its own brand line rather than
  // reusing `app.name`.
  auth: {
    brand: 'SLUSK',
    loginSubtitle: 'sign in to continue',
    // States plainly that this is a fresh install, not a returning user —
    // deliberately not "welcome" copy.
    setupSubtitle: 'first run — create the account that will manage this install',
    // Candid, not reassuring: v1 has no reset flow (see CLAUDE.md's
    // "never invent data" — the mock's "forgot password?" link is dropped
    // rather than shipped pointing nowhere).
    setupWarning: 'No password reset. Recovering access means deleting the user row from the database.',
    loginHeader: 'Login',
    setupHeader: 'Create account',
    usernameLabel: 'USERNAME',
    passwordLabel: 'PASSWORD',
    confirmPasswordLabel: 'CONFIRM PASSWORD',
    usernamePlaceholder: 'admin',
    passwordPlaceholder: '••••••••',
    signIn: '[↵] SIGN IN',
    createAccount: '[↵] CREATE ACCOUNT',
    submitting: 'working…',
    usernameRequired: 'username and password required',
    passwordTooShort: 'password must be at least 8 characters',
    passwordTooLong: 'password must be 72 bytes or fewer',
    passwordMismatch: 'passwords do not match',
    genericError: 'something went wrong — try again',
    footer: 'slusk · lidarr companion · self-hosted',
    logout: 'log out',
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
