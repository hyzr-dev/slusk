import { useCharts, useShares, useStatus, useUploads } from '../api/queries';
import CumulativeAreaChart from '../components/charts/CumulativeAreaChart';
import PassBarChart from '../components/charts/PassBarChart';
import EmptyState from '../components/tui/EmptyState';
import SectionHeader from '../components/tui/SectionHeader';
import { formatTime } from '../format';
import { t } from '../strings';
import styles from './Health.module.css';

export default function Health() {
  const { data: status, isPending: statusPending } = useStatus();
  const { data: charts } = useCharts();
  const { data: uploads } = useUploads();
  const { data: shares } = useShares();

  // moduleDetails is keyed by pipeline module name (wanted_sync, discovery,
  // selecting, downloading, importing), not by external dependency — there is
  // no dependency-health surface yet (issue #200) — so these cards describe
  // pipeline modules generically rather than a hardcoded module→dependency map.
  const modules = status?.moduleDetails ?? {};
  const names = Object.keys(modules).sort();

  // Human-readable counters, not Prometheus metric names — see strings.ts
  // health.metricsHeading for why the mock's slskdarr_* row labels aren't used.
  const metricRows = [
    { key: t.health.metricActive, value: status?.active ?? 0 },
    { key: t.health.metricQueued, value: status?.queued ?? 0 },
    { key: t.health.metricStalled, value: status?.stalled ?? 0 },
    { key: t.health.metricOrphaned, value: status?.orphaned ?? 0 },
    { key: t.health.metricUploads, value: uploads?.active ?? 0 },
    { key: t.health.metricShared, value: shares?.files ?? 0 },
  ];

  return (
    <>
      <div className={styles.depGrid}>
        {statusPending ? (
          // The first fetch hasn't resolved yet, so `names` being empty here
          // means "not established", not "there are no modules" — those are
          // different facts and must not render the same message. Matches
          // the placeholder idiom Shares.tsx and Setup.tsx use while their
          // own first fetch is in flight.
          <div className={styles.placeholder}>{t.jobs.loading}</div>
        ) : names.length === 0 ? (
          <EmptyState message={t.health.empty} />
        ) : (
          names.map((name) => {
            const m = modules[name];
            const label = m.lastAttempt ? formatTime(m.lastAttempt) : t.health.neverRun;
            return (
              <div key={name} className={styles.depCard}>
                <div className={styles.depHead}>
                  <span className={m.ready ? styles.dotOk : styles.dotBad}>■</span>
                  <span className={styles.depName}>{name.replace(/_/g, ' ')}</span>
                  <span className={styles.spacer} />
                  <span className={m.ready ? styles.stateOk : styles.stateBad}>
                    {m.ready ? t.health.ready : t.health.notReady}
                  </span>
                </div>
                <div
                  className={`${styles.depDetail} ${m.ready ? '' : styles.unhealthy}`}
                  title={m.lastError}
                >
                  {label}
                  {m.consecutiveFailures > 0 &&
                    ` (${t.health.consecutiveFailures(m.consecutiveFailures)})`}
                </div>
              </div>
            );
          })
        )}
      </div>

      <div className={styles.chartsGrid}>
        <div className={styles.chartCol}>
          <SectionHeader label={t.health.reconcileRateHeading} meta={t.health.reconcileRateMeta} />
          <div className={styles.chartBody}>
            <PassBarChart passes={charts?.passes ?? []} />
          </div>
        </div>
        <div>
          <SectionHeader label={t.health.completedHeading} meta={t.health.completedMeta} />
          <div className={styles.chartBody}>
            <CumulativeAreaChart buckets={charts?.completedByHour ?? []} />
          </div>
        </div>
      </div>

      <SectionHeader label={t.health.metricsHeading} meta={t.health.metricsMeta} />
      <div>
        {metricRows.map((row) => (
          <div key={row.key} className={styles.metricRow}>
            <span className={styles.metricKey}>{row.key}</span>
            <span className={styles.metricValue}>{row.value}</span>
          </div>
        ))}
      </div>
    </>
  );
}
