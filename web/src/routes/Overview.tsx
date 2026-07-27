import { useNavigate } from 'react-router-dom';
import { useAllJobs, useCharts, useStatus } from '../api/queries';
import type { Job } from '../api/types';
import ThroughputAreaChart from '../components/charts/ThroughputAreaChart';
import EmptyState from '../components/tui/EmptyState';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import Tag from '../components/tui/Tag';
import Ticks, { type TickTone } from '../components/tui/Ticks';
import { formatEta, formatShortTime, formatSize, formatSpeed, percent } from '../format';
import { t } from '../strings';
import { sortJobs } from './jobSort';
import styles from './Overview.module.css';

// At most this many rows in the TRANSFERS panel — matches the mock
// (docs/design/slskdarr-tui.dc.html:102) rather than the full jobs list.
// Jobs are ranked active-before-stalled, oldest-first within each group,
// before slicing (#233, 'transferOrder' in jobSort.ts) — max_active defaults
// to 30 (config.example.toml), so the active|stalled set routinely exceeds
// this cap, and this ranking is what decides who gets cut: a stalled job
// never displaces an active one, however long it's been stalled.
const MAX_TRANSFER_ROWS = 8;
// At most this many rows in the RECONCILE list.
const MAX_RECONCILE_ROWS = 7;
// Per-row tick resolution in TRANSFERS, matching the mock exactly.
const TRANSFER_TICKS = 104;
// How many recent throughput samples feed the ACTIVE stat cell's sparkline.
const SPARKLINE_SAMPLES = 20;

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
  const jobsQuery = useAllJobs();
  const statusQuery = useStatus();
  const chartsQuery = useCharts();
  const jobs = jobsQuery.data ?? [];
  const status = statusQuery.data;
  const charts = chartsQuery.data;

  // Three independent polls feeding three independent regions: a dead
  // /api/charts must not blank the transfer list, and a dead /api/jobs must
  // not blank the counters. So each region gates on what it actually reads
  // rather than on one primary query (issue #201).
  const statusPhase = queryPhase(statusQuery);
  const jobsPhase = queryPhase(jobsQuery);
  const chartsPhase = queryPhase(chartsQuery);

  // Sparklines are drawn only for ACTIVE: it's the one cell with a real
  // series to plot (charts.throughput). The other four stat cells have no
  // per-status history anywhere in the backend, and the mock's sparklines
  // for them are generated noise — omitted rather than faked (see spec,
  // Overview section).
  const statCells = [
    { label: t.status.active, value: status?.active ?? 0, phase: statusPhase },
    { label: t.status.queued, value: status?.queued ?? 0, phase: statusPhase },
    { label: t.status.stalled, value: status?.stalled ?? 0, phase: statusPhase },
    { label: t.status.parked, value: status?.parked ?? 0, phase: statusPhase },
    { label: t.status.done, value: jobs.filter((j) => j.status === 'done').length, phase: jobsPhase },
  ];

  // ACTIVE remains download-only: it counts pipeline download jobs and its
  // sparkline therefore follows only the download series. Uploads get their
  // own independently-scaled chart below rather than changing this stat.
  const throughput = charts?.throughput ?? [];
  const uploadThroughput = charts?.uploadThroughput ?? [];
  const sparklineSamples = throughput.slice(-SPARKLINE_SAMPLES);
  const sparklinePeak = Math.max(1, ...sparklineSamples.map((s) => s.bytesPerSecond));

  // Ranked active-before-stalled, then createdAt ascending within each group
  // (see 'transferOrder' in jobSort.ts) — rather than the backend's own
  // updated_at DESC ordering, so a row stays in place for its whole lifetime
  // instead of jumping to the top on every progress update, and a long-stalled
  // job can never permanently occupy a slot that an active transfer needs (#233).
  const transferRows = sortJobs(
    jobs.filter((j) => j.status === 'active' || j.status === 'stalled'),
    'transferOrder',
    'asc',
  ).slice(0, MAX_TRANSFER_ROWS);

  // SearchPass carries no id of its own, and the window this slices from is
  // capped at 20 and slides — a position-based number would look like a
  // stable reference but silently point at a different pass on the next
  // poll, so no id column is shown at all (see spec, Overview section).
  const reconcileRows = (charts?.passes ?? []).slice(-MAX_RECONCILE_ROWS).reverse();

  return (
    <>
      <QueryNotice phase={statusPhase} />
      <div className={styles.statGrid}>
        {statCells.map((cell, i) => (
          <div key={cell.label} className={styles.statCell}>
            <div className={styles.statLabel}>{cell.label}</div>
            <div className={styles.statValue}>{hasData(cell.phase) ? cell.value : '—'}</div>
            {i === 0 && hasData(chartsPhase) && sparklineSamples.length > 0 && (
              <div className={styles.sparkline}>
                {sparklineSamples.map((sample, j) => (
                  <span
                    key={j}
                    style={{ height: `${Math.max(8, (sample.bytesPerSecond / sparklinePeak) * 100)}%` }}
                  />
                ))}
              </div>
            )}
          </div>
        ))}
      </div>

      <div className={styles.mainGrid}>
        <div className={styles.transfers}>
          <SectionHeader
            label={t.overview.transfersHeading}
            // "0 active" is a claim, not a placeholder — omit the meta until
            // /status has answered. SectionHeader skips a falsy meta.
            meta={hasData(statusPhase) ? t.overview.activeCountMeta(status?.active ?? 0) : undefined}
          />
          <QueryNotice phase={jobsPhase} />
          {hasData(jobsPhase) &&
            (transferRows.length === 0 ? (
              <EmptyState message={t.overview.empty} />
            ) : (
              transferRows.map((job) => {
                // A non-zero queuePosition means the job is waiting in a peer's
                // remote queue: it's still 'active' but no bytes are moving,
                // so the byte counts below are replaced and the tick bar must
                // not flare as if data were arriving.
                const queued = Boolean(job.queuePosition);
                const pct = percent(job.bytesDone, job.bytesTotal);
                const tone = tickTone(job);
                const live = job.status === 'active' && !job.queuePosition;
                const right = queued
                  ? t.overview.queuePos(job.queuePosition ?? 0)
                  : job.state === 'IMPORTING'
                    ? t.jobs.verifying
                    : `${formatSize(job.bytesDone)} / ${formatSize(job.bytesTotal)}`;

                return (
                  <div
                    key={job.id}
                    className={styles.transferRow}
                    onClick={() => navigate(`/jobs/${job.id}`)}
                  >
                    <div className={styles.transferHead}>
                      <Tag status={job.status} state={job.state} queuePosition={job.queuePosition} />
                      <span className={styles.transferTitle}>{job.title}</span>
                      <span className={styles.transferSpeed}>{formatSpeed(job.speed)}</span>
                      <span
                        className={`${styles.transferPct} ${queued ? styles.pctQueued : tone === 'bad' ? styles.pctBad : styles.pctBar}`}
                      >
                        {pct}%
                      </span>
                    </div>
                    <div className={styles.transferTicks}>
                      <Ticks percent={pct} count={TRANSFER_TICKS} tone={tone} live={live} height={12} />
                    </div>
                    <div className={styles.transferSub}>
                      <span>{job.artist} · {job.peer || '—'}</span>
                      <span>{right}</span>
                    </div>
                  </div>
                );
              })
            ))}
        </div>

        <div>
          <SectionHeader label={t.overview.throughputHeading} />
          <QueryNotice phase={chartsPhase} />
          <div className={styles.throughputBody}>
            {hasData(chartsPhase) && (
              <>
                <ThroughputAreaChart samples={throughput} direction="download" />
                <ThroughputAreaChart samples={uploadThroughput} direction="upload" />
              </>
            )}
          </div>

          <SectionHeader label={t.chrome.reconcile} />
          {hasData(chartsPhase) &&
            (reconcileRows.length === 0 ? (
              <EmptyState message={t.overview.noChartData} />
            ) : (
              <div className={styles.reconcileList}>
                {reconcileRows.map((pass) => {
                  const matched = pass.matched > 0;
                  const durationSeconds = Math.round(
                    (new Date(pass.finishedAt).getTime() - new Date(pass.startedAt).getTime()) / 1000,
                  );
                  return (
                    <div key={pass.startedAt} className={styles.reconcileRow}>
                      <span className={styles.reconcileTime}>{formatShortTime(pass.finishedAt)}</span>
                      <span className={styles.reconcileSpacer} />
                      <span className={matched ? styles.reconcileMatch : styles.reconcileNoMatch}>
                        {matched ? t.overview.reconcileMatched(pass.matched) : t.overview.reconcileNoMatch}
                      </span>
                      <span className={styles.reconcileDur}>{formatEta(durationSeconds)}</span>
                    </div>
                  );
                })}
              </div>
            ))}
        </div>
      </div>
    </>
  );
}
