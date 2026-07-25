import { useEffect, useState } from 'react';
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
 * Pure derivation of the LIVE cell's label from TanStack Query's
 * `dataUpdatedAt` (ms since epoch of the last successful fetch) and the
 * current time, both as plain numbers so this is testable without waiting on
 * a real clock or mocking timers.
 *
 * `dataUpdatedAt` is 0 before the first successful fetch ever completes —
 * treated the same as "just updated" rather than computing a nonsense
 * multi-billion-second age from the epoch.
 */
export function elapsedLabel(dataUpdatedAt: number, now: number): string {
  if (!dataUpdatedAt) return t.chrome.updatedNow;
  const seconds = Math.floor((now - dataUpdatedAt) / 1000);
  if (seconds < 1) return t.chrome.updatedNow;
  return t.chrome.updatedAgo(seconds);
}

/**
 * Ticks the LIVE cell's label once a second so a stalled poll is visible as
 * a number that keeps climbing, matching StatusBar's `useClock` shape.
 */
function useElapsedLabel(dataUpdatedAt: number): string {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  return elapsedLabel(dataUpdatedAt, now);
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
  const liveLabel = useElapsedLabel(status.dataUpdatedAt);

  return (
    <div className={styles.bar}>
      <div className={`${styles.cell} ${styles.brand}`}>
        <span className={styles.brandName}>{t.app.name.toUpperCase()}</span>
      </div>

      <div className={styles.cell}>
        <span className={status.isError ? styles.dotStale : styles.dot} />
        <span className={styles.quiet}>{t.chrome.live}</span>
        <span>{liveLabel}</span>
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
