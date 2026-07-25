import { useEffect, useState } from 'react';
import { useLocation, useParams } from 'react-router-dom';
import { useConfig, useJobs, useStatus } from '../api/queries';
import type { ModuleStatus } from '../api/types';
import { parseGoDuration } from '../goDuration';
import { t } from '../strings';
import styles from './Header.module.css';

interface Heading {
  title: string;
  subtitle: string;
}

// Route -> page title/subtitle. /jobs/:id is handled separately below since
// its subtitle needs the id from the URL, not a static string.
function heading(pathname: string, jobId: string | undefined): Heading {
  switch (true) {
    case pathname === '/':
      return t.header.overview;
    case pathname === '/jobs':
      return t.header.jobs;
    case pathname.startsWith('/jobs/'):
      return {
        title: t.header.jobDetail.title,
        subtitle: jobId ? t.header.jobDetail.subtitleWithId(jobId) : '',
      };
    case pathname === '/events':
      return t.header.events;
    case pathname === '/peers':
      return t.header.peers;
    case pathname === '/shares':
      return t.header.shares;
    case pathname === '/health':
      return t.header.health;
    case pathname === '/settings':
      return t.header.settings;
    default:
      return { title: t.app.name, subtitle: '' };
  }
}

// The reconcile countdown text, or null when it cannot be computed —
// wanted_sync has never completed, or wantedSyncInterval is missing/not a
// parseable Go duration. Callers must render nothing (not "NaN") in that case.
function reconcileText(
  wantedSync: ModuleStatus | undefined,
  wantedSyncInterval: string | undefined,
  now: number,
): string | null {
  if (!wantedSync?.lastCompleted) return null;
  const intervalSeconds = parseGoDuration(wantedSyncInterval);
  if (intervalSeconds === null) return null;
  const lastCompletedMs = new Date(wantedSync.lastCompleted).getTime();
  if (Number.isNaN(lastCompletedMs)) return null;
  const dueInSeconds = (lastCompletedMs + intervalSeconds * 1000 - now) / 1000;
  if (dueInSeconds <= 0) return t.header.reconcileDueNow;
  return t.header.reconcileIn(Math.max(1, Math.round(dueInSeconds / 60)));
}

export default function Header() {
  const location = useLocation();
  const { id } = useParams();
  const { dataUpdatedAt } = useJobs();
  const { data: status } = useStatus();
  const { data: config } = useConfig();

  // Ticks once a second so "updated Ns ago" and the reconcile countdown stay
  // current between poll intervals, without inventing a second source of
  // truth for freshness — dataUpdatedAt already comes from the jobs query.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const page = heading(location.pathname, id);
  const updatedSeconds = dataUpdatedAt ? Math.max(0, Math.round((now - dataUpdatedAt) / 1000)) : null;
  const reconcile = reconcileText(status?.moduleDetails.wanted_sync, config?.pipeline.wantedSyncInterval, now);

  return (
    <header className={styles.header}>
      <div className={styles.titleBlock}>
        {/* Not an <h1>: every route already renders its own PageHeading h1
            with the same text (see e.g. routes/Jobs.tsx), and this sticky
            chrome header is present on every page alongside it. A second,
            identically-named top-level heading would be a real accessibility
            regression (two landmarks announcing "Jobs") and breaks
            getByRole('heading', ...) uniqueness in the existing route tests
            (App.test.tsx, Shares.test.tsx) — reworking every route to drop
            its own heading was out of scope for this change. */}
        <div className={styles.title}>{page.title}</div>
        <div className={styles.subtitle}>{page.subtitle}</div>
      </div>

      <div className={styles.liveBadge}>
        <span className={styles.liveDot} aria-hidden="true" />
        <span className={styles.liveLabel}>{t.header.live}</span>
        {updatedSeconds !== null && (
          <span className={styles.updated}>{t.header.updatedAgo(updatedSeconds)}</span>
        )}
      </div>

      {reconcile && (
        <div className={styles.reconcileBadge} title={t.header.reconcileTooltip}>
          <span className={styles.reconcileIcon} aria-hidden="true" />
          <span>{reconcile}</span>
        </div>
      )}
    </header>
  );
}
