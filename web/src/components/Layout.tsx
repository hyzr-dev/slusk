import { Outlet, useLocation } from 'react-router-dom';
import { useConversations, useStatus } from '../api/queries';
import { t } from '../strings';
import { FlashProvider } from './chrome/FlashContext';
import SideNav from './chrome/SideNav';
import type { NavGroup } from './chrome/SideNav';
import StatusBar from './chrome/StatusBar';
import TopBar from './chrome/TopBar';
import styles from './Layout.module.css';

export default function Layout() {
  const status = useStatus();
  const conversations = useConversations();

  const s = status.data;
  const inFlight = (s?.active ?? 0) + (s?.queued ?? 0) + (s?.stalled ?? 0);
  const needsAttention = (s?.stalled ?? 0) + (s?.parked ?? 0);
  const unreadChat = (conversations.data ?? []).reduce((sum, c) => sum + c.unread, 0);

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

  // Every top-level route now renders its own visible <h1> via the <Page>
  // shell (the 27 July TUI restyle, #281) — a second, hidden one here would
  // just duplicate it. JobDetail is the one route with no <Page> of its own
  // (it isn't a top-level nav destination, and its SectionHeader already
  // renders the album title as an <h2>), so it's still the one case that
  // needs a synthesized, visually-hidden <h1> to give the document exactly
  // one heading at that level. Named after the parent 'jobs' nav entry
  // (matching the sidebar link a user actually followed to get there) rather
  // than the job title, which lives one level down as the <h2> instead.
  const location = useLocation();
  const isJobDetail = /^\/jobs\/[^/]+\/?$/.test(location.pathname);

  return (
    <FlashProvider>
      <div className={styles.app}>
        <TopBar />
        <div className={styles.body}>
          <SideNav groups={groups} />
          <main className={styles.main}>
            {isJobDetail && <h1 className={styles.visuallyHidden}>{t.nav.jobs}</h1>}
            <Outlet />
          </main>
        </div>
        <StatusBar />
      </div>
    </FlashProvider>
  );
}
