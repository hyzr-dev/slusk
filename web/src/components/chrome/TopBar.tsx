import { useCharts, useJobs, useStatus } from '../../api/queries';
import { formatShortTime, formatSpeed } from '../../format';
import { t } from '../../strings';
import styles from './TopBar.module.css';

/**
 * Aggregate download speed across jobs that are actually moving bytes.
 *
 * A job with a peer queue position is "active" but transferring nothing, and
 * counting its stale `speed` would inflate the headline figure.
 */
function totalSpeed(jobs: { status: string; speed?: number; queuePosition?: number }[]): number {
  return jobs
    .filter((j) => j.status === 'active' && !j.queuePosition)
    .reduce((sum, j) => sum + (j.speed ?? 0), 0);
}

/**
 * The top rule: brand, live-poll indicator, last reconcile pass and
 * aggregate download speed.
 *
 * There is deliberately no per-dependency indicator here (no Lidarr/Soulseek
 * dot) — StatusReport.modules is keyed by pipeline module name
 * (wanted_sync, discovery, …), not by dependency, so there is no key to read
 * that would mean "Lidarr" or "Soulseek". See internal/observ/observ.go and
 * its test asserting on ModuleDetails["wanted_sync"]/["discovery"].
 */
export default function TopBar() {
  const jobs = useJobs();
  const status = useStatus();
  const charts = useCharts();

  const down = totalSpeed(jobs.data ?? []);
  const lastPass = charts.data?.passes?.at(-1);

  return (
    <div className={styles.bar}>
      <div className={`${styles.cell} ${styles.brand}`}>
        <span className={styles.brandName}>{t.app.name.toUpperCase()}</span>
      </div>

      <div className={styles.cell}>
        <span className={status.isError ? styles.dotStale : styles.dot} />
        <span className={styles.quiet}>{t.chrome.live}</span>
        <span>{status.isFetching ? t.chrome.updatedNow : ''}</span>
      </div>

      <div className={styles.cell}>
        {t.chrome.reconcile}{' '}
        <span className={styles.quiet}>
          {lastPass ? formatShortTime(lastPass.finishedAt) : t.chrome.reconcileNever}
        </span>
      </div>

      <div className={styles.cell}>
        {t.chrome.down} <span className={styles.value}>{down ? formatSpeed(down) : t.chrome.idle}</span>
      </div>

      <span className={styles.spacer} />
    </div>
  );
}
