import { useEffect, useState } from 'react';
import { useLocation, useParams } from 'react-router-dom';
import { useConfig, useJobDetail, useJobs, useStatus } from '../api/queries';
import type { Job, JobDetail, ModuleStatus } from '../api/types';
import { parseGoDuration } from '../goDuration';
import { t } from '../strings';
import styles from './Header.module.css';

interface Heading {
  title: string;
  subtitle: string;
}

// Route -> static page title/subtitle. /jobs/:id is handled separately in
// jobDetailHeading below since its title needs live job data, not a static
// string.
function heading(pathname: string): Heading {
  switch (true) {
    case pathname === '/':
      return t.header.overview;
    case pathname === '/jobs':
      return t.header.jobs;
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

// The job-detail route's title needs live data, not a static string — this
// is the sole surviving purpose of the per-route heading this diff removes
// everywhere else (see routes/JobDetail.tsx, which used to render its own
// PageHeading for exactly this reason). Mirrors JobDetail's own three-tier
// fallback: the live job list, then the cached detail response, then a
// loading placeholder — so the header and the page body never disagree
// about which tier they're in.
function jobDetailHeading(
  jobs: Job[],
  jobIdParam: string | undefined,
  detail: JobDetail | undefined,
  detailReady: boolean,
): Heading {
  const numericId = Number(jobIdParam);
  const job = jobs.find((j) => j.id === numericId);
  const subtitle = jobIdParam ? t.header.jobDetail.subtitleWithId(jobIdParam) : '';
  if (job) return { title: job.title, subtitle };
  if (detailReady && detail) return { title: detail.title, subtitle };
  return { title: t.jobs.loading, subtitle: '' };
}

export default function Header() {
  const location = useLocation();
  const { id } = useParams();
  const isJobDetail = location.pathname.startsWith('/jobs/');

  const { data: jobs = [], dataUpdatedAt } = useJobs();
  const { data: status } = useStatus();
  const { data: config } = useConfig();
  // Only fetches on the job-detail route (see useJobDetail's `enabled`
  // param) — Header renders on every route, unlike JobDetail itself.
  const numericId = Number(id);
  const {
    data: detail,
    isPlaceholderData: detailIsPlaceholder,
  } = useJobDetail(numericId, isJobDetail && !Number.isNaN(numericId));
  const detailReady = detail !== undefined && !detailIsPlaceholder;

  // Ticks once a second so "updated Ns ago" and the reconcile countdown stay
  // current between poll intervals, without inventing a second source of
  // truth for freshness — dataUpdatedAt already comes from the jobs query.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const page = isJobDetail ? jobDetailHeading(jobs, id, detail, detailReady) : heading(location.pathname);
  const updatedSeconds = dataUpdatedAt ? Math.max(0, Math.round((now - dataUpdatedAt) / 1000)) : null;
  const reconcile = reconcileText(status?.moduleDetails.wanted_sync, config?.pipeline.wantedSyncInterval, now);
  const reconcileLabel = reconcile ? `${reconcile} — ${t.header.reconcileTooltip}` : undefined;

  return (
    <header className={styles.header}>
      <div className={styles.titleBlock}>
        {/* The sole <h1> on the page: every route used to render its own
            PageHeading with the same text, which was a real accessibility
            regression (two landmarks announcing "Jobs") — see #181 review.
            Routes no longer render a heading of their own. */}
        <h1 className={styles.title}>{page.title}</h1>
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
        // `title` alone is a hover-only tooltip, invisible to keyboard and
        // touch users; aria-label duplicates the explanation into the
        // accessible name so screen readers get it even without a pointer.
        <div className={styles.reconcileBadge} title={t.header.reconcileTooltip} aria-label={reconcileLabel}>
          <span className={styles.reconcileIcon} aria-hidden="true" />
          <span>{reconcile}</span>
        </div>
      )}
    </header>
  );
}
