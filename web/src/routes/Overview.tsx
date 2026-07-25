import { useNavigate } from 'react-router-dom';
import type { Job } from '../api/types';
import { useCharts, useJobs } from '../api/queries';
import ChartCard from '../components/charts/ChartCard';
import CumulativeAreaChart from '../components/charts/CumulativeAreaChart';
import PassBarChart from '../components/charts/PassBarChart';
import PageHeading from '../components/PageHeading';
import SourceBadge from '../components/SourceBadge';
import StatCard from '../components/StatCard';
import { formatSpeed, percent } from '../format';
import { t } from '../strings';
import styles from './Overview.module.css';

// A row's phase/state below the progress bar, and the matching value on the
// right (mock: docs/design/slskdarr-dashboard.dc.html lines 158-166,
// 1105/1114). Mirrors the queue/importing/stalled precedence Jobs.tsx uses
// for its own pill and progress sub-line.
function rowPhase(job: Job, inQueue: boolean, pct: number): { phase: string; meta: string; dotClass: string } {
  if (job.state === 'IMPORTING') {
    return { phase: t.overview.phaseImporting, meta: t.overview.metaVerifying, dotClass: styles.dotImporting };
  }
  if (inQueue) {
    return {
      phase: t.overview.phaseQueue(job.queuePosition!),
      meta: t.overview.metaQueue(job.queuePosition!),
      dotClass: styles.dotActive,
    };
  }
  if (job.status === 'stalled') {
    return { phase: t.overview.phaseStalled, meta: formatSpeed(job.speed), dotClass: styles.dotStalled };
  }
  return { phase: `${pct}%`, meta: formatSpeed(job.speed), dotClass: styles.dotActive };
}

export default function Overview() {
  const navigate = useNavigate();
  const { data: jobs = [] } = useJobs();
  const { data: charts } = useCharts();

  const activeCount = jobs.filter((j) => j.status === 'active').length;
  const importingCount = jobs.filter((j) => j.state === 'IMPORTING').length;
  const downloadingCount = activeCount - importingCount;
  const queuedCount = jobs.filter((j) => j.status === 'queued').length;
  const stalledCount = jobs.filter((j) => j.status === 'stalled').length;
  const orphanedCount = jobs.filter((j) => j.status === 'orphaned').length;
  const failedCount = jobs.filter((j) => j.status === 'failed').length;
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
      <PageHeading>{t.nav.overview}</PageHeading>

      <div className={styles.hero}>
        <div className={styles.heroLeft}>
          <div className={styles.heroLabel}>{t.overview.heroLabel}</div>
          <div className={styles.heroSummary}>
            {t.overview.heroSummary(downloadingCount, completed24h, queuedCount)}
          </div>
        </div>
        <div className={styles.heroPills}>
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
              const { phase, meta, dotClass } = rowPhase(j, inQueue, pct);
              return (
                <div
                  key={j.id}
                  className={styles.row}
                  onClick={() => navigate(`/jobs/${j.id}`)}
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
                        className={`${styles.rowBarFill} ${dotClass}`}
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
