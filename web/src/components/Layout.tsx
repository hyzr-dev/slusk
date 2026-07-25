import { NavLink, Outlet } from 'react-router-dom';
import { useJobs, useShares, useStatus } from '../api/queries';
import type { ModuleStatus, StatusReport } from '../api/types';
import Header from './Header';
import styles from './Layout.module.css';
import { t } from '../strings';

interface NavItem {
  to: string;
  label: string;
  end: boolean;
}

interface NavGroup {
  label: string;
  items: NavItem[];
}

// Search (#58) and Setup (#63) are deliberately absent — neither is built
// yet, and a dead link is worse than an incomplete sidebar.
const NAV_GROUPS: NavGroup[] = [
  {
    label: t.nav.groupMonitor,
    items: [
      { to: '/', label: t.nav.overview, end: true },
      { to: '/jobs', label: t.nav.jobs, end: false },
      { to: '/events', label: t.nav.events, end: false },
      { to: '/health', label: t.nav.health, end: false },
    ],
  },
  {
    label: t.nav.groupSoulseek,
    items: [
      { to: '/shares', label: t.nav.shares, end: false },
      { to: '/peers', label: t.nav.peers, end: false },
    ],
  },
  {
    label: t.nav.groupSystem,
    items: [{ to: '/settings', label: t.nav.settings, end: false }],
  },
];

type HealthState = 'ok' | 'warn' | 'unknown';

// A module's dependency-dot state, distinguishing three cases the old
// `ready ?? true` logic conflated into two:
//   - unknown: the /status query hasn't loaded yet, the backend didn't
//     report this module at all, or the module has never completed a first
//     tick since process start (lastAttempt === "") — none of these mean
//     "healthy", but they don't mean "down" either.
//   - warn: the module has attempted at least once and is not ready.
//   - ok: the module is ready.
function moduleHealth(status: StatusReport | undefined, module: ModuleStatus | undefined): HealthState {
  if (!status || !module || !module.lastAttempt) return 'unknown';
  return module.ready ? 'ok' : 'warn';
}

// Combines two modules' health into one dependency row: warn wins over
// unknown, which wins over ok, so a single struggling module is never
// masked by a healthy sibling.
function combineHealth(a: HealthState, b: HealthState): HealthState {
  if (a === 'warn' || b === 'warn') return 'warn';
  if (a === 'unknown' || b === 'unknown') return 'unknown';
  return 'ok';
}

function healthMeta(state: HealthState): string {
  if (state === 'ok') return t.nav.depHealthy;
  if (state === 'warn') return t.nav.depUnhealthy;
  return t.nav.depUnknown;
}

export default function Layout() {
  const { data: status } = useStatus();
  const { data: jobs = [] } = useJobs();
  // Polled at the same 15s cadence as the Shares route itself (SHARES_INTERVAL
  // in api/queries.ts) rather than a separate, slower interval — the sidebar
  // only reads `enabled` and a folder count from this response, but keeping
  // one interval for the one query key avoids two components disagreeing on
  // how fresh "fresh" means for the same data.
  const { data: shares } = useShares();

  const jobsBadge = jobs.filter(
    (j) => j.status === 'active' || j.status === 'queued' || j.status === 'stalled',
  ).length;

  // Lidarr and Soulseek have no dedicated health endpoint (issue #181): their
  // dot is inferred from the pipeline module that talks to them, which is a
  // proxy, not a direct signal. wanted_sync is the only module that calls
  // Lidarr. Soulseek is depended on by both discovery (issues searches) and
  // downloading (transfers) — selecting itself only ranks already-found
  // candidates and does not touch the network.
  const modules = status?.moduleDetails ?? {};
  const lidarrState = moduleHealth(status, modules.wanted_sync);
  const soulseekState = combineHealth(
    moduleHealth(status, modules.discovery),
    moduleHealth(status, modules.downloading),
  );

  // SharesReport.enabled (not folders.length, which is [] both when sharing
  // is off and when it's on with nothing configured) is what distinguishes
  // "native Soulseek sharing is off" — a normal setup, not a fault — from
  // "sharing is on but no folders are configured" — a real warning, since
  // sharing is the price of admission to the Soulseek network.
  const shareFolders = shares?.folders.length ?? 0;
  const sharesState: HealthState = !shares ? 'unknown' : !shares.enabled ? 'unknown' : shareFolders > 0 ? 'ok' : 'warn';
  const sharesMeta = !shares
    ? t.nav.depUnknown
    : !shares.enabled
      ? t.nav.depDisabled
      : `${healthMeta(sharesState)} · ${t.nav.depFolders(shareFolders)}`;

  const deps: { name: string; state: HealthState; meta: string }[] = [
    { name: t.nav.depLidarr, state: lidarrState, meta: healthMeta(lidarrState) },
    { name: t.nav.depSoulseek, state: soulseekState, meta: healthMeta(soulseekState) },
    { name: t.nav.depShares, state: sharesState, meta: sharesMeta },
  ];

  return (
    <div className={styles.app}>
      <aside className={styles.sidebar}>
        <div className={styles.brand}>
          <div className={styles.brandMark}>{t.app.mark}</div>
          <div>
            <div className={styles.brandName}>{t.app.name}</div>
            <div className={styles.brandTagline}>{t.app.tagline}</div>
          </div>
        </div>

        <nav className={styles.nav} aria-label={t.nav.navAriaLabel}>
          {NAV_GROUPS.map((group) => {
            const groupHeadingId = `nav-group-${group.label.toLowerCase()}`;
            return (
              <div key={group.label}>
                <div className={styles.groupLabel} id={groupHeadingId}>{group.label}</div>
                <ul className={styles.groupList} aria-labelledby={groupHeadingId}>
                  {group.items.map((item) => (
                    <li key={item.to}>
                      <NavLink
                        to={item.to}
                        end={item.end}
                        className={({ isActive }) => (isActive ? styles.navItemActive : styles.navItem)}
                      >
                        <span className={styles.navLabel}>{item.label}</span>
                        {item.to === '/jobs' && jobsBadge > 0 && (
                          <span className={styles.badge} aria-label={t.nav.jobsBadgeLabel(jobsBadge)}>
                            {jobsBadge}
                          </span>
                        )}
                      </NavLink>
                    </li>
                  ))}
                </ul>
              </div>
            );
          })}
        </nav>

        <div className={styles.deps}>
          {deps.map((d) => (
            <div key={d.name} className={styles.depRow}>
              <span
                className={`${styles.depDot} ${
                  d.state === 'ok' ? styles.depOk : d.state === 'warn' ? styles.depWarn : styles.depUnknown
                }`}
                aria-hidden="true"
              />
              <span className={styles.depName}>{d.name}</span>
              <span className={styles.depMeta}>{d.meta}</span>
            </div>
          ))}
        </div>
      </aside>
      <div className={styles.mainCol}>
        <Header />
        <main className={styles.main}>
          <Outlet />
        </main>
      </div>
    </div>
  );
}
