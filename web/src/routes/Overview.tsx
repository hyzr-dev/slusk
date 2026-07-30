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
import { formatDuration, formatShortTime, formatSize, formatSpeed, percent } from '../format';
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

export default function Overview() {
  const navigate = useNavigate();
  const jobsQuery = useJobs({ page: 0, filter: 'inflight', sort: 'transfer', dir: 'asc', source: 'all', q: '', pageSize: TRANSFER_PAGE_SIZE });
  const statusQuery = useStatus();
  const chartsQuery = useCharts();
  const result = jobsQuery.data;
  const transferRows = result?.jobs ?? [];
  const status = statusQuery.data;
  const charts = chartsQuery.data;

  // Scopes the SSE connection to exactly these rows (issue #268), the same
  // mechanism Jobs.tsx uses for its page — this is what lets the backend's
  // job-delta bookkeeping stay bounded for the Overview surface too, not
  // just the paged Jobs list (see #258's accumulator work).
  useJobScope(transferRows.map((job) => job.id));

  // Three independent polls feeding three independent regions: a dead
  // /api/charts must not blank the transfer list, and a dead /api/jobs must
  // not blank the counters. So each region gates on what it actually reads
  // rather than on one primary query (issue #201).
  const statusPhase = queryPhase(statusQuery);
  const jobsPhase = queryPhase(jobsQuery);
  const chartsPhase = queryPhase(chartsQuery);

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
                    className={`${styles.transferGrid} ${styles.transferRow}`}
                    onClick={() => navigate(`/jobs/${job.id}`)}
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
