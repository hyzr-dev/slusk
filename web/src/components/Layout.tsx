import { Outlet } from 'react-router-dom';
import { useStatus } from '../api/queries';
import { t } from '../strings';
import { FlashProvider } from './chrome/FlashContext';
import SideNav from './chrome/SideNav';
import type { NavGroup } from './chrome/SideNav';
import StatusBar from './chrome/StatusBar';
import TopBar from './chrome/TopBar';
import styles from './Layout.module.css';

export default function Layout() {
  const status = useStatus();

  const s = status.data;
  const inFlight = (s?.active ?? 0) + (s?.queued ?? 0) + (s?.stalled ?? 0);
  const needsAttention = (s?.stalled ?? 0) + (s?.orphaned ?? 0);
  const unreadChat = 0; // no messages API yet; see #183

  const groups: NavGroup[] = [
    {
      label: t.nav.groupMonitor,
      items: [
        { to: '/', label: t.nav.overview, end: true },
        { to: '/jobs', label: t.nav.jobs, badge: inFlight },
        { to: '/events', label: t.nav.events },
        { to: '/peers', label: t.nav.peers },
        { to: '/health', label: t.nav.health, badge: needsAttention, alert: true },
      ],
    },
    {
      label: t.nav.groupSoulseek,
      items: [
        { to: '/search', label: t.nav.search },
        { to: '/shares', label: t.nav.shares },
        { to: '/chat', label: t.nav.chat, badge: unreadChat },
      ],
    },
    {
      label: t.nav.groupSystem,
      items: [
        { to: '/setup', label: t.nav.setup },
        { to: '/settings', label: t.nav.settings },
      ],
    },
  ];

  return (
    <FlashProvider>
      <div className={styles.app}>
        <TopBar />
        <div className={styles.body}>
          <SideNav groups={groups} />
          <main className={styles.main}>
            <Outlet />
          </main>
        </div>
        <StatusBar />
      </div>
    </FlashProvider>
  );
}
