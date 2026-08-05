import { useCharts, useShares, useStatus, useUploads } from '../api/queries';
import CumulativeAreaChart from '../components/charts/CumulativeAreaChart';
import PassBarChart from '../components/charts/PassBarChart';
import EmptyState from '../components/tui/EmptyState';
import Page from '../components/tui/Page';
import Panel from '../components/tui/Panel';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import { formatTime } from '../format';
import { t } from '../strings';
import styles from './Health.module.css';

export default function Health() {
  const statusQuery = useStatus();
  const chartsQuery = useCharts();
  const uploadsQuery = useUploads();
  const sharesQuery = useShares();
  const status = statusQuery.data;
  const charts = chartsQuery.data;

  const chartsPhase = queryPhase(chartsQuery);
  const metricsPhase = queryPhase(statusQuery, uploadsQuery, sharesQuery);

  // moduleDetails is keyed by pipeline module name (wanted_sync, discovery,
  // selecting, downloading, importing), not by external dependency — there is
  // no dependency-health surface yet (issue #200) — so these cards describe
  // pipeline modules generically rather than a hardcoded module→dependency map.
  const modules = status?.moduleDetails ?? {};
  const names = Object.keys(modules).sort();

  // Human-readable counters, not Prometheus metric names — see strings.ts
  // health.metricsHeading for why the mock's slusk_* row labels aren't used.
  // Each row carries the phase of the query its value came from, so a failed
  // uploads/shares poll doesn't blank the status-sourced rows next to it.
  // Also feeds the dependency-modules region below — named after its source
  // rather than either consumer.
  const statusPhase = queryPhase(statusQuery);
  //
  // The seven status-sourced rows are ordered the way a job moves through the
  // pipeline — wanted → selecting → waiting → queued → active — so the list
  // reads as a progression rather than an alphabet, with the two rows that
  // mean "something is wrong" (stalled, parked) last.
  const metricRows = [
    { key: t.health.metricWanted, value: status?.wanted ?? 0, phase: statusPhase },
    { key: t.health.metricSelecting, value: status?.selecting ?? 0, phase: statusPhase },
    { key: t.health.metricWaiting, value: status?.waiting ?? 0, phase: statusPhase },
    { key: t.health.metricQueued, value: status?.queued ?? 0, phase: statusPhase },
    { key: t.health.metricActive, value: status?.active ?? 0, phase: statusPhase },
    { key: t.health.metricStalled, value: status?.stalled ?? 0, phase: statusPhase },
    { key: t.health.metricParked, value: status?.parked ?? 0, phase: statusPhase },
    { key: t.health.metricUploads, value: uploadsQuery.data?.active ?? 0, phase: queryPhase(uploadsQuery) },
    { key: t.health.metricShared, value: sharesQuery.data?.files ?? 0, phase: queryPhase(sharesQuery) },
  ];

  return (
    <Page title={t.page.health.title} subtitle={t.page.health.subtitle}>
      <QueryNotice phase={statusPhase} />
      {hasData(statusPhase) &&
        (names.length === 0 ? (
          <EmptyState message={t.health.empty} />
        ) : (
          <div className={styles.depGrid}>
            {names.map((name) => {
              const m = modules[name];
              const label = m.lastAttempt ? formatTime(m.lastAttempt) : t.health.neverRun;
              return (
                <Panel key={name} className={styles.depCard}>
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
                </Panel>
              );
            })}
          </div>
        ))}

      <QueryNotice phase={chartsPhase} />
      <div className={styles.chartsGrid}>
        <Panel>
          <SectionHeader label={t.health.reconcileRateHeading} meta={t.health.reconcileRateMeta} />
          <div className={styles.chartBody}>
            {hasData(chartsPhase) && <PassBarChart passes={charts?.passes ?? []} />}
          </div>
        </Panel>
        <Panel>
          <SectionHeader label={t.health.completedHeading} meta={t.health.completedMeta} />
          <div className={styles.chartBody}>
            {hasData(chartsPhase) && <CumulativeAreaChart buckets={charts?.completedByHour ?? []} />}
          </div>
        </Panel>
      </div>

      <Panel>
        <SectionHeader label={t.health.metricsHeading} meta={t.health.metricsMeta} />
        <QueryNotice phase={metricsPhase} />
        {metricRows.map((row) => (
          <div key={row.key} className={styles.metricRow}>
            <span className={styles.metricKey}>{row.key}</span>
            <span className={styles.metricValue}>{hasData(row.phase) ? row.value : '—'}</span>
          </div>
        ))}
      </Panel>
    </Page>
  );
}
