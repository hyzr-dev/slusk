import { useEffect, useState } from 'react';
import { useLogout, useSession } from '../../api/auth';
import { STATUS_INTERVAL, useCharts, useLiveData, useStatus } from '../../api/queries';
import { formatAge, formatShortTime, formatSpeed } from '../../format';
import { t } from '../../strings';
import styles from './TopBar.module.css';

// Two missed polls. One late response is normal jitter and must not raise an
// alarm; two in a row means the data on screen has genuinely stopped being
// refreshed.
const STALE_AFTER_MS = STATUS_INTERVAL * 2;

/**
 * The LIVE cell's label, or null while the poll is keeping up.
 *
 * Returns null rather than an age string in the healthy case on purpose: a
 * counter that climbs to four and resets carries no information during normal
 * operation, and a cell that only ever shows digits when something is wrong is
 * one the eye can trust. Pure and taking `now` as an argument so it is
 * testable without a real clock.
 *
 * `dataUpdatedAt` is 0 before the first successful fetch ever completes — that
 * is "nothing to report yet", not "stale", so it also returns null.
 */
export function stalenessLabel(
  dataUpdatedAt: number,
  now: number,
  staleAfterMs: number = STALE_AFTER_MS,
): string | null {
  if (!dataUpdatedAt) return null;
  const ageMs = now - dataUpdatedAt;
  if (ageMs < staleAfterMs) return null;
  return t.chrome.stale(formatAge(Math.floor(ageMs / 1000)));
}

/**
 * Re-evaluates the staleness label once a second, so a poll that stops
 * answering surfaces on its own rather than waiting for some other render.
 * Matches StatusBar's `useClock` shape.
 */
function useStalenessLabel(dataUpdatedAt: number): string | null {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  return stalenessLabel(dataUpdatedAt, now);
}

/**
 * The top rule: brand, live-poll indicator, last reconcile pass and
 * global download and upload speeds.
 *
 * There is deliberately no per-dependency indicator here (no Lidarr/Soulseek
 * dot) — StatusReport.modules is keyed by pipeline module name
 * (wanted_sync, discovery, …), not by dependency, so there is no key to read
 * that would mean "Lidarr" or "Soulseek". See internal/observ/observ.go and
 * its test asserting on ModuleDetails["wanted_sync"]/["discovery"].
 */
export default function TopBar() {
  const status = useStatus();
  const charts = useCharts();
  const live = useLiveData();
  const session = useSession();
  const logout = useLogout();

  // Resolve each direction independently: a frame can carry one newly-added
  // field while the other still falls back to REST during a rolling update.
  // Nullish coalescing is deliberate — an explicit stream zero means idle and
  // must never revive the last non-zero REST sample.
  const latestDown = charts.data?.throughput.at(-1)?.bytesPerSecond ?? 0;
  const latestUp = charts.data?.uploadThroughput.at(-1)?.bytesPerSecond ?? 0;
  const down = live?.down ?? latestDown;
  const up = live?.up ?? latestUp;
  const lastPass = charts.data?.passes?.at(-1);
  const stale = useStalenessLabel(status.dataUpdatedAt);

  return (
    <div
      className={styles.bar}
      role="region"
      aria-label={t.chrome.statusRegion}
      tabIndex={0}
    >
      <div className={`${styles.cell} ${styles.brand}`}>
        <span className={styles.brandName}>{t.app.name.toUpperCase()}</span>
        {/* Rendered only when the server reports one. A server predating
            issue #229 omits the field, and an empty slot next to the name
            reads as a bug rather than as absent information. */}
        {status.data?.version && (
          <span className={styles.brandVersion}>{status.data.version}</span>
        )}
      </div>

      <div className={styles.cell}>
        <span className={stale ? styles.dotStale : styles.dot} />
        <span className={stale ? styles.bad : styles.quiet}>{stale ?? t.chrome.live}</span>
      </div>

      <div className={styles.cell}>
        {t.chrome.reconcile}{' '}
        <span className={styles.quiet}>
          {lastPass ? formatShortTime(lastPass.finishedAt) : t.chrome.reconcileNever}
        </span>
      </div>

      <div className={styles.cell}>
        <span>{t.chrome.down} <span className={styles.value}>{down ? formatSpeed(down) : t.chrome.idle}</span></span>{' '}
        <span aria-hidden="true">{t.chrome.throughputSeparator}</span>{' '}
        <span>{t.chrome.up} <span className={styles.value}>{up ? formatSpeed(up) : t.chrome.idle}</span></span>
      </div>

      <span className={styles.spacer} />

      {/* Username is omitted, not rendered as an empty gap, when the request
          authenticated via the machine bearer token rather than a session
          cookie (`make dev` against a fresh lab DB) — see SessionResponse's
          doc comment. The logout control itself still renders either way. */}
      <div className={`${styles.cell} ${styles.account}`}>
        {session.data?.username && (
          <span className={styles.username}>{session.data.username}</span>
        )}
        <button
          type="button"
          className={styles.logoutButton}
          onClick={() => logout.mutate()}
          disabled={logout.isPending}
        >
          {t.auth.logout}
        </button>
      </div>
    </div>
  );
}
