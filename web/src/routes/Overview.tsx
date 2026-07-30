import type { KeyboardEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { useCharts, useJobs, useStatus } from '../api/queries';
import { useJobScope } from '../api/stream';
import type { Job } from '../api/types';
import ThroughputAreaChart from '../components/charts/ThroughputAreaChart';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import Tag from '../components/tui/Tag';
import Ticks, { type TickTone } from '../components/tui/Ticks';
import { formatAge, formatDuration, formatShortTime, formatSize, formatSpeed, percent } from '../format';
import { t } from '../strings';
import styles from './Overview.module.css';

// Rows in the TRANSFERS panel — matches the mock
// (docs/design/slskdarr-tui.dc.html:105) rather than the full jobs list.
// Selection, ordering and this row count are all server-side (issue #268):
// filter=inflight is every job the pipeline holds a MaxActive slot for —
// state DOWNLOADING or IMPORTING (issue #287 widened this from the old
// active+stalled 'transferring' union, which dropped a job the moment it
// stopped moving bytes). sort=transfer ranks active, then stalled, then
// waiting, then importing, and orders by createdAt ascending inside a group.
// The client renders result.jobs exactly as returned — no filter, sort or
// slice here.
const TRANSFER_PAGE_SIZE = 8;
// At most this many rows in the RECONCILE list.
const MAX_RECONCILE_ROWS = 7;

// Rows in the RECENTLY FINISHED panel. Selection is server-side: filter=finished
// is state DONE or FAILED with an updated_at inside the backend's window
// (store.DashboardFinishedWindow), sort=recent is newest finish first. The panel
// reads neither total nor facets, so it opts out of them (skipFacets) — the facet
// query is the expensive half of /api/jobs and runs whatever the filter is
// (issue #286).
const FINISHED_PAGE_SIZE = 5;

// Rows in the FAILED IMPORTS panel (#310). Selection is server-side:
// filter=failed is every job whose current status is 'failed', time-unbounded
// — unlike filter=finished above, which is windowed to
// store.DashboardFinishedWindow, a failure a caller hasn't dealt with yet is
// still worth surfacing regardless of when it happened. sort=recent is
// newest-failure-first. The panel opts out of facets (skipFacets) for the
// same reason as the finished panel: it reads neither total nor chips.
const FAILED_PAGE_SIZE = 8;

/**
 * Tick colour for a TRANSFERS row: queued takes priority over stalled since a
 * job can carry a stale queuePosition into a terminal status, but never while
 * still counted as 'active' here (see Tag.tagFor for the same precedence).
 */
function tickTone(job: Job): TickTone {
  if (job.queuePosition) return 'queued';
  if (job.status === 'stalled') return 'bad';
  return 'bar';
}

/**
 * Age of a finished job, for the WHEN column. Reads updated_at, which is the
 * completion stamp for DONE and FAILED — MarkJobFailed is guarded against
 * re-failing an already-terminal job and the wanted-sync metadata backfill
 * leaves updated_at alone past WANTED, so it never moves again once set.
 * Returns an em dash for a missing or unparseable value rather than a
 * misleading "0s".
 */
function finishedAge(updatedAt: string): string {
  if (!updatedAt) return '—';
  const ms = Date.now() - new Date(updatedAt).getTime();
  if (Number.isNaN(ms)) return '—';
  return formatAge(Math.max(0, Math.floor(ms / 1000)));
}

/**
 * Enter/Space activation for a whole clickable row (issue #292).
 *
 * The row itself is the keyboard target here, on purpose — unlike Jobs.tsx,
 * where the row deliberately is NOT the target. A Jobs row holds two actions
 * (the details-toggle button and the title link), and making the row itself
 * handle Enter once broke native Enter-on-link activation: the row's keydown
 * handler ran preventDefault() for every Enter, including one bubbling up
 * from a focused nested <Link>, cancelling the browser's own navigation
 * before it fired (see the regression comment in Jobs.test.tsx). An Overview
 * row has exactly one action and no nested interactive element, so that trap
 * does not apply — do not "unify" this with Jobs' pattern.
 */
function handleRowKeyDown(event: KeyboardEvent, onActivate: () => void) {
  if (event.key !== 'Enter' && event.key !== ' ') return;
  event.preventDefault(); // Space would otherwise scroll the page
  onActivate();
}

export default function Overview() {
  const navigate = useNavigate();
  const jobsQuery = useJobs({ page: 0, filter: 'inflight', sort: 'transfer', dir: 'asc', source: 'all', q: '', pageSize: TRANSFER_PAGE_SIZE });
  const statusQuery = useStatus();
  const chartsQuery = useCharts();
  const finishedQuery = useJobs({
    page: 0, filter: 'finished', sort: 'recent', dir: 'desc',
    source: 'all', q: '', pageSize: FINISHED_PAGE_SIZE, skipFacets: true,
  });
  const failedQuery = useJobs({
    page: 0, filter: 'failed', sort: 'recent', dir: 'desc',
    source: 'all', q: '', pageSize: FAILED_PAGE_SIZE, skipFacets: true,
  });
  const result = jobsQuery.data;
  const transferRows = result?.jobs ?? [];
  const status = statusQuery.data;
  const charts = chartsQuery.data;
  const finishedRows = finishedQuery.data?.jobs ?? [];
  const failedRows = failedQuery.data?.jobs ?? [];

  // Only the in-flight rows: a finished job is terminal, so the stream never
  // sends deltas for it and there is no reason to make the backend track it.
  // This does NOT make the finished panel immune to live replacement: useJobs
  // runs replaceLiveJobPage over every page it returns regardless of scope,
  // so a job that just transitioned to DONE can still be rendered from its
  // last in-flight live frame until framedAt ages past LIVE_JOB_FRESH_MS.
  useJobScope(transferRows.map((job) => job.id));

  // Three independent polls feeding three independent regions: a dead
  // /api/charts must not blank the transfer list, and a dead /api/jobs must
  // not blank the counters. So each region gates on what it actually reads
  // rather than on one primary query (issue #201).
  const statusPhase = queryPhase(statusQuery);
  const jobsPhase = queryPhase(jobsQuery);
  const chartsPhase = queryPhase(chartsQuery);
  const finishedPhase = queryPhase(finishedQuery);
  const failedPhase = queryPhase(failedQuery);

  // IMPORTED 24H sums /api/charts' completedByHour, exactly 24 zero-filled
  // hourly buckets ending at the current hour (ChartsReport, api/types.ts).
  // Two things are non-obvious about it:
  //  1. The buckets count attempt_succeeded events (job_events), not jobs
  //     currently in state DONE — deliberately: the event count is
  //     monotonic and historical, while a state count changes retroactively
  //     if a job later leaves DONE.
  //  2. The window is 23 whole hours plus the current partial hour, i.e. a
  //     rolling day rather than a calendar day from midnight — hence the
  //     label '24h' rather than the mock's 'IMPORTED TODAY' (mock line 116).
  const importedCount = (charts?.completedByHour ?? []).reduce((sum, hour) => sum + hour.count, 0);
  const importingCount = hasData(jobsPhase) ? (result?.facets.status.importing ?? 0) : 0;
  const stalled = status?.stalled ?? 0;
  const parked = status?.parked ?? 0;
  const attention = stalled + parked;

  const statCells = [
    {
      label: t.overview.statInFlight,
      value: status?.active ?? 0,
      sub: t.overview.subInFlight(status?.active ?? 0),
      phase: statusPhase,
    },
    {
      label: t.overview.statQueued,
      value: status?.queued ?? 0,
      sub: t.overview.subQueued,
      phase: statusPhase,
      tone: 'dim' as const,
    },
    {
      // Value comes from /api/charts, not /api/jobs — see the comment above.
      label: t.overview.statImported,
      value: importedCount,
      sub: t.overview.subImported(importingCount),
      phase: chartsPhase,
    },
    {
      label: t.overview.statAttention,
      value: attention,
      sub: t.overview.subAttention(stalled, parked),
      phase: statusPhase,
      tone: attention > 0 ? ('bad' as const) : ('dim' as const),
    },
  ];

  const throughput = charts?.throughput ?? [];
  const uploadThroughput = charts?.uploadThroughput ?? [];
  // One shared axis row underneath both charts (mock line 173) rather than
  // one per chart — whichever direction has samples names the left edge.
  const axisSample = throughput[0] ?? uploadThroughput[0];

  // SearchPass carries no id of its own, and the window this slices from is
  // capped at 20 and slides — a position-based number would look like a
  // stable reference but silently point at a different pass on the next
  // poll, so no id column is shown at all (see spec, Overview section).
  const reconcileRows = (charts?.passes ?? []).slice(-MAX_RECONCILE_ROWS).reverse();

  return (
    <Page title={t.page.overview.title} subtitle={t.page.overview.subtitle}>
      <QueryNotice phase={statusPhase} />
      <div className={styles.statGrid}>
        {statCells.map((cell) => (
          <Panel key={cell.label} className={styles.statCell}>
            <div className={styles.statLabel}>{cell.label}</div>
            <div
              className={
                cell.tone === 'dim'
                  ? `${styles.statValue} ${styles.statValueDim}`
                  : cell.tone === 'bad'
                    ? `${styles.statValue} ${styles.statValueBad}`
                    : styles.statValue
              }
            >
              {hasData(cell.phase) ? cell.value : '—'}
            </div>
            <div className={styles.statSub}>{cell.sub}</div>
          </Panel>
        ))}
      </div>

      <Panel>
        <SectionHeader
          label={t.overview.transfersHeading}
          // A count is a claim, not a placeholder — omit the meta until
          // /api/jobs has answered. SectionHeader skips a falsy meta.
          // total comes from the same response as the rows, so it can reveal
          // the rows this fixed-height panel could not fit.
          meta={
            hasData(jobsPhase)
              ? (result?.total ?? 0) > transferRows.length
                ? t.overview.inFlightTruncatedMeta(transferRows.length, result?.total ?? 0)
                : t.overview.inFlightCountMeta(transferRows.length)
              : undefined
          }
        />
        <QueryNotice phase={jobsPhase} />
        {hasData(jobsPhase) &&
          (transferRows.length === 0 ? (
            <EmptyState message={t.overview.empty} />
          ) : (
            <div role="table">
              <div role="row" className={`${styles.transferGrid} ${styles.transferHead}`}>
                <span role="columnheader">{t.overview.gridHead.status}</span>
                <span role="columnheader">{t.overview.gridHead.album}</span>
                <span role="columnheader">{t.overview.gridHead.peer}</span>
                <span role="columnheader">{t.overview.gridHead.progress}</span>
                <span role="columnheader" className={styles.headRight}>{t.overview.gridHead.speed}</span>
                <span role="columnheader" className={styles.headRight}>{t.overview.gridHead.size}</span>
              </div>
              {transferRows.map((job) => {
                // A non-zero queuePosition means the job is waiting in a peer's
                // remote queue: it's still 'active' but no bytes are moving,
                // so the byte counts below are replaced and the tick bar must
                // not flare as if data were arriving.
                const queued = Boolean(job.queuePosition);
                const pct = percent(job.bytesDone, job.bytesTotal);
                const tone = tickTone(job);
                const live = job.status === 'active' && !job.queuePosition;
                const size = queued
                  ? t.overview.queuePosShort(job.queuePosition ?? 0)
                  : job.state === 'IMPORTING'
                    ? t.jobs.verifying
                    : `${formatSize(job.bytesDone)} / ${formatSize(job.bytesTotal)}`;

                return (
                  <div
                    key={job.id}
                    role="row"
                    tabIndex={0}
                    className={`${styles.transferGrid} ${styles.transferRow}`}
                    onClick={() => navigate(`/jobs/${job.id}`)}
                    onKeyDown={(event) => handleRowKeyDown(event, () => navigate(`/jobs/${job.id}`))}
                  >
                    <span role="cell">
                      <Tag status={job.status} queuePosition={job.queuePosition} bare />
                    </span>
                    <span role="cell" className={styles.albumCell}>
                      <span className={styles.transferTitle}>{job.title}</span>
                      <span className={styles.transferArtist}>{job.artist}</span>
                      {job.format && <span className={styles.transferFormat}>{job.format}</span>}
                    </span>
                    <span role="cell" className={styles.peerCell}>{job.peer || '—'}</span>
                    <span role="cell" className={styles.progressCell}>
                      <Ticks percent={pct} tone={tone} live={live} height={8} />
                      <span
                        className={`${styles.pct} ${queued ? styles.pctQueued : tone === 'bad' ? styles.pctBad : styles.pctBar}`}
                      >
                        {pct}%
                      </span>
                    </span>
                    <span role="cell" className={styles.right}>{queued ? '—' : formatSpeed(job.speed)}</span>
                    <span role="cell" className={styles.right}>{size}</span>
                  </div>
                );
              })}
            </div>
          ))}
      </Panel>

      <Panel>
        <SectionHeader label={t.overview.finishedHeading} />
        <QueryNotice phase={finishedPhase} />
        {hasData(finishedPhase) &&
          (finishedRows.length === 0 ? (
            <EmptyState message={t.overview.noneFinished} />
          ) : (
            <div role="table">
              <div role="row" className={`${styles.finishedGrid} ${styles.transferHead}`}>
                <span role="columnheader">{t.overview.finishedGridHead.status}</span>
                <span role="columnheader">{t.overview.finishedGridHead.album}</span>
                <span role="columnheader" className={styles.peerCell}>{t.overview.finishedGridHead.peer}</span>
                <span role="columnheader" className={styles.headRight}>{t.overview.finishedGridHead.when}</span>
              </div>
              {finishedRows.map((job) => (
                <div
                  key={job.id}
                  role="row"
                  tabIndex={0}
                  className={`${styles.finishedGrid} ${styles.transferRow}`}
                  onClick={() => navigate(`/jobs/${job.id}`)}
                  onKeyDown={(event) => handleRowKeyDown(event, () => navigate(`/jobs/${job.id}`))}
                >
                  <span role="cell">
                    <Tag status={job.status} bare />
                  </span>
                  <span role="cell" className={styles.albumCell}>
                    <span className={styles.transferTitle}>{job.title}</span>
                    <span className={styles.transferArtist}>{job.artist}</span>
                  </span>
                  <span role="cell" className={styles.peerCell}>{job.peer || '—'}</span>
                  <span role="cell" className={styles.finishedWhen}>{finishedAge(job.updatedAt)}</span>
                </div>
              ))}
            </div>
          ))}
      </Panel>

      <Panel>
        <SectionHeader label={t.overview.failedHeading} />
        <QueryNotice phase={failedPhase} />
        {hasData(failedPhase) &&
          (failedRows.length === 0 ? (
            <EmptyState message={t.overview.noneFailed} />
          ) : (
            <div role="table">
              <div role="row" className={`${styles.failedGrid} ${styles.transferHead}`}>
                <span role="columnheader">{t.overview.failedGridHead.status}</span>
                <span role="columnheader">{t.overview.failedGridHead.album}</span>
                <span role="columnheader">{t.overview.failedGridHead.reason}</span>
                <span role="columnheader" className={styles.headRight}>{t.overview.failedGridHead.when}</span>
              </div>
              {failedRows.map((job) => {
                const reason = job.failDetail || job.failReason || '—';
                return (
                  <div
                    key={job.id}
                    role="row"
                    tabIndex={0}
                    className={`${styles.failedGrid} ${styles.transferRow}`}
                    onClick={() => navigate(`/jobs/${job.id}`)}
                    onKeyDown={(event) => handleRowKeyDown(event, () => navigate(`/jobs/${job.id}`))}
                  >
                    <span role="cell">
                      <Tag status={job.status} bare />
                    </span>
                    <span role="cell" className={styles.albumCell}>
                      <span className={styles.transferTitle}>{job.title}</span>
                      <span className={styles.transferArtist}>{job.artist}</span>
                    </span>
                    <span role="cell" className={styles.failedReason} title={reason}>{reason}</span>
                    <span role="cell" className={styles.finishedWhen}>{finishedAge(job.updatedAt)}</span>
                  </div>
                );
              })}
            </div>
          ))}
      </Panel>

      <div className={styles.mainGrid}>
        <Panel>
          <SectionHeader label={t.chrome.reconcile} />
          {hasData(chartsPhase) &&
            (reconcileRows.length === 0 ? (
              <EmptyState message={t.overview.noChartData} />
            ) : (
              <div role="table">
                <div role="row" className={`${styles.reconcileGrid} ${styles.reconcileHead}`}>
                  <span role="columnheader">{t.overview.reconcileGridHead.when}</span>
                  <span role="columnheader">{t.overview.reconcileGridHead.result}</span>
                  <span role="columnheader" className={styles.headRight}>{t.overview.reconcileGridHead.dur}</span>
                </div>
                {reconcileRows.map((pass) => {
                  const matched = pass.matched > 0;
                  // Deliberately not rounded here — formatDuration owns the
                  // precision, and rounding first would collapse every
                  // sub-second pass to 0 before it ever got the chance.
                  const durationSeconds =
                    (new Date(pass.finishedAt).getTime() - new Date(pass.startedAt).getTime()) / 1000;
                  return (
                    <div key={pass.startedAt} role="row" className={styles.reconcileGrid}>
                      <span role="cell" className={styles.reconcileTime}>{formatShortTime(pass.finishedAt)}</span>
                      <span role="cell" className={matched ? styles.reconcileMatch : styles.reconcileNoMatch}>
                        {matched ? t.overview.reconcileMatched(pass.matched) : t.overview.reconcileNoMatch}
                      </span>
                      <span role="cell" className={styles.reconcileDur}>{formatDuration(durationSeconds)}</span>
                    </div>
                  );
                })}
              </div>
            ))}
        </Panel>

        <Panel>
          <SectionHeader label={t.overview.throughputHeading} />
          {hasData(chartsPhase) && (
            <div className={styles.throughputBody}>
              <ThroughputAreaChart samples={throughput} direction="download" showAxis={false} />
              <ThroughputAreaChart samples={uploadThroughput} direction="upload" showAxis={false} />
              {axisSample && (
                <div className={styles.throughputAxis}>
                  <span>{formatShortTime(axisSample.at)}</span>
                  <span>{t.overview.chartRangeEnd}</span>
                </div>
              )}
            </div>
          )}
        </Panel>
      </div>
    </Page>
  );
}
