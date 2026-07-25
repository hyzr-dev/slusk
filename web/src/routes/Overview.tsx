import { useNavigate } from 'react-router-dom';
import type { Job } from '../api/types';
import { useCharts, useJobs } from '../api/queries';
import ChartCard from '../components/charts/ChartCard';
import CumulativeAreaChart from '../components/charts/CumulativeAreaChart';
import PassBarChart from '../components/charts/PassBarChart';
import SourceBadge from '../components/SourceBadge';
import StatCard from '../components/StatCard';
import { formatEta, formatSpeed, percent } from '../format';
import { t } from '../strings';
import { countByStatus } from './jobFilter';
import styles from './Overview.module.css';

// A row's phase/state below the progress bar, and the matching value on the
// right (mock: docs/design/slskdarr-dashboard.dc.html lines 158-166,
// 1105/1114). Mirrors the queue/importing/stalled precedence Jobs.tsx uses
// for its own pill and progress sub-line. inQueue is derived here, not
// passed in, so the job.queuePosition! assertion below is sound by local
// reasoning rather than by caller convention.
function rowPhase(job: Job, pct: number): { phase: string; meta: string; dotClass: string; hatched: boolean } {
  if (job.state === 'IMPORTING') {
    return {
      phase: t.overview.phaseImporting,
      meta: t.overview.metaVerifying,
      dotClass: styles.dotImporting,
      hatched: false,
    };
  }
  const inQueue = job.status === 'active' && (job.queuePosition ?? 0) > 0;
  if (inQueue) {
    // A peer-queued job has transferred zero bytes — a hatched neutral fill
    // (not a solid colour) keeps it from reading as complete or actively
    // downloading, matching the mock's barBg for inQueue rows. The dot and
    // phase text use --stalled (dotStalled), not --active, since this job
    // isn't moving yet (mock's pctColor for inQueue: var(--stalled)).
    return {
      phase: t.overview.phaseQueue(job.queuePosition!),
      meta: formatEta(job.etaSeconds),
      dotClass: styles.dotStalled,
      hatched: true,
    };
  }
  if (job.status === 'stalled') {
    return { phase: t.overview.phaseStalled, meta: formatSpeed(job.speed), dotClass: styles.dotStalled, hatched: false };
  }
  return { phase: `${pct}%`, meta: formatSpeed(job.speed), dotClass: styles.dotActive, hatched: false };
}

export default function Overview() {
  const navigate = useNavigate();
  const { data: jobs = [] } = useJobs();
  const { data: charts } = useCharts();

  // countByStatus (jobFilter.ts) already implements this exact bucketing —
  // including the "importing is a state-level refinement of active" split —
  // so Overview reuses it instead of re-running the same filter passes
  // Jobs.tsx and Layout.tsx already run.
  const counts = countByStatus(jobs, '', 'all');
  const importingCount = counts.importing;
  // Direct filter, not activeCount - importingCount: that subtraction is
  // only correct while dashboardStatus() maps IMPORTING to the "active"
  // status, and goes negative silently if that invariant ever changes.
  // counts.active is already jobs with status active and state !== IMPORTING.
  const downloadingCount = counts.active;
  const activeCount = counts.active + counts.importing;
  const queuedCount = counts.queued;
  const stalledCount = counts.stalled;
  const orphanedCount = counts.orphaned;
  const failedCount = counts.failed;
  // The backend zero-fills completedByHour to exactly 24 UTC-pinned hourly
  // buckets (see api/types.ts ChartsReport) — summing them, rather than a
  // client-side "since local midnight" cutoff, is what avoids the
  // client/server day-boundary bug from issue #88.
  const completed24h = (charts?.completedByHour ?? []).reduce((sum, b) => sum + b.count, 0);
  const needsYouCount = stalledCount + failedCount;

  const activeDownloads = jobs.filter((j) => j.status === 'active' || j.status === 'stalled');
  const totalSpeed = jobs
    .filter((j) => j.status === 'active' && (j.queuePosition ?? 0) === 0)
    .reduce((sum, j) => sum + (j.speed ?? 0), 0);

  return (
    <>
      <div className={styles.hero}>
        <div className={styles.heroLeft}>
          <div className={styles.heroLabel}>{t.overview.heroLabel}</div>
          <div className={styles.heroSummary}>
            {t.overview.heroSummary(downloadingCount, completed24h, queuedCount)}
          </div>
        </div>
        <div className={styles.heroPills} role="group" aria-label={t.overview.heroPillsLabel}>
          <div className={styles.pill}>
            <span className={`${styles.pillDot} ${styles.dotActive}`} aria-hidden="true" />
            <span className={styles.pillValue}>{downloadingCount}</span>
            <span className={styles.pillLabel}>{t.overview.pillDownloading}</span>
          </div>
          <div className={styles.pill}>
            <span className={`${styles.pillDot} ${styles.dotImporting}`} aria-hidden="true" />
            <span className={styles.pillValue}>{importingCount}</span>
            <span className={styles.pillLabel}>{t.overview.pillImporting}</span>
          </div>
          <div className={styles.pill}>
            <span className={`${styles.pillDot} ${styles.dotStalled}`} aria-hidden="true" />
            <span className={styles.pillValue}>{needsYouCount}</span>
            <span className={styles.pillLabel}>{t.overview.pillNeedsYou}</span>
          </div>
        </div>
      </div>

      <div className={styles.cards}>
        <StatCard
          label={t.status.active}
          value={activeCount}
          dotColor="var(--active)"
          sub={importingCount > 0 ? t.overview.statActiveSubImporting(importingCount) : t.overview.statActiveSub}
        />
        <StatCard
          label={t.status.queued}
          value={queuedCount}
          dotColor="var(--queued)"
          sub={t.overview.statQueuedSub}
        />
        <StatCard
          label={t.status.stalled}
          value={stalledCount}
          dotColor="var(--stalled)"
          sub={t.overview.statStalledSub}
        />
        {/* Restored per #87 (see the removed dashboard comment this diff had
            dropped): the legacy dashboard omitted the failed card even
            though it counted the status; showing it is a deliberate fix, not
            an addition, and folding failed into the "needs you" pill alone
            makes the failed/stalled split invisible. */}
        <StatCard
          label={t.status.failed}
          value={failedCount}
          dotColor="var(--failed)"
          sub={t.overview.statFailedSub}
        />
        <StatCard
          label={t.status.orphaned}
          value={orphanedCount}
          dotColor="var(--orphaned)"
          sub={t.overview.statOrphanedSub}
        />
        <StatCard
          label={t.overview.statCompletedLabel}
          value={completed24h}
          dotColor="var(--done)"
          sub={t.overview.statCompletedSub}
        />
      </div>

      <div className={styles.activePanel}>
        <div className={styles.panelHeader}>
          <div className={styles.panelTitle}>{t.overview.activeDownloadsTitle}</div>
          <div className={styles.throughput}>
            {totalSpeed > 0 ? formatSpeed(totalSpeed) : t.overview.throughputIdle}
          </div>
        </div>
        {activeDownloads.length === 0 ? (
          <div className={styles.empty}>{t.overview.empty}</div>
        ) : (
          <div>
            {activeDownloads.map((j) => {
              const inQueue = j.status === 'active' && (j.queuePosition ?? 0) > 0;
              const pct = percent(j.bytesDone, j.bytesTotal);
              const { phase, meta, dotClass, hatched } = rowPhase(j, pct);
              const openJob = () => navigate(`/jobs/${j.id}`);
              return (
                <div
                  key={j.id}
                  className={styles.row}
                  role="link"
                  tabIndex={0}
                  onClick={openJob}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      openJob();
                    }
                  }}
                >
                  <span className={`${styles.rowDot} ${dotClass}`} aria-hidden="true" />
                  <div className={styles.rowMain}>
                    <div className={styles.rowTitleLine}>
                      <span className={styles.rowTitle}>{j.title}</span>
                      <SourceBadge source={j.source} />
                    </div>
                    <div className={styles.rowSub}>
                      {j.artist} · <span className={styles.rowPeer}>{j.peer || '—'}</span>
                    </div>
                  </div>
                  <div className={styles.rowProgress}>
                    <div className={styles.rowBar}>
                      <div
                        className={`${styles.rowBarFill} ${hatched ? styles.rowBarHatched : dotClass}`}
                        style={{ width: `${inQueue ? 100 : Math.max(2, pct)}%` }}
                      />
                    </div>
                    <div className={styles.rowMeta}>
                      <span>{phase}</span>
                      <span>{meta}</span>
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <div className={styles.charts}>
        <ChartCard title={t.overview.chartPasses}>
          <PassBarChart passes={charts?.passes ?? []} />
        </ChartCard>
        <ChartCard title={t.overview.chartCompleted}>
          <CumulativeAreaChart buckets={charts?.completedByHour ?? []} />
        </ChartCard>
      </div>
    </>
  );
}
