import { NavLink, Outlet } from 'react-router-dom';
import { useJobs, useShares, useStatus } from '../api/queries';
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

export default function Layout() {
  const { data: status } = useStatus();
  const { data: jobs = [] } = useJobs();
  const { data: shares } = useShares();

  const jobsBadge = jobs.filter(
    (j) => j.status === 'active' || j.status === 'queued' || j.status === 'stalled',
  ).length;

  // Lidarr and Soulseek have no dedicated health endpoint (issue #181): their
  // dot is inferred from the pipeline module that talks to them, which is a
  // proxy, not a direct signal — a module can read healthy simply because it
  // has not ticked since the dependency actually died. wanted_sync is the
  // only module that calls Lidarr; downloading/selecting both depend on a
  // working Soulseek connection. Default to healthy (not "down") while the
  // status query is still loading, so the sidebar doesn't flash a false
  // warning on every page load.
  const modules = status?.moduleDetails ?? {};
  const lidarrHealthy = modules.wanted_sync ? modules.wanted_sync.ready : true;
  const soulseekHealthy =
    (modules.downloading ? modules.downloading.ready : true) &&
    (modules.selecting ? modules.selecting.ready : true);
  const shareFolders = shares?.folders.length ?? 0;
  const sharesHealthy = shareFolders > 0;

  const deps = [
    { name: t.nav.depLidarr, healthy: lidarrHealthy, meta: lidarrHealthy ? t.nav.depHealthy : t.nav.depUnhealthy },
    { name: t.nav.depSoulseek, healthy: soulseekHealthy, meta: soulseekHealthy ? t.nav.depHealthy : t.nav.depUnhealthy },
    { name: t.nav.depShares, healthy: sharesHealthy, meta: t.nav.depFolders(shareFolders) },
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

        <nav className={styles.nav}>
          {NAV_GROUPS.map((group) => (
            <div key={group.label}>
              <div className={styles.groupLabel}>{group.label}</div>
              {group.items.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) => (isActive ? styles.navItemActive : styles.navItem)}
                >
                  <span className={styles.navLabel}>{item.label}</span>
                  {item.to === '/jobs' && jobsBadge > 0 && (
                    <span className={styles.badge}>{jobsBadge}</span>
                  )}
                </NavLink>
              ))}
            </div>
          ))}
        </nav>

        <div className={styles.deps}>
          {deps.map((d) => (
            <div key={d.name} className={styles.depRow}>
              <span className={`${styles.depDot} ${d.healthy ? styles.depOk : styles.depWarn}`} aria-hidden="true" />
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
