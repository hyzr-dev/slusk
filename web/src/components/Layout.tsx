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

  // SectionHeader renders each panel's own label as an <h2>, but nothing
  // upstream of that gave the document a top-level heading once PageHeading
  // was removed from every route — Events and Peers use neither, so they had
  // no heading at all. A single <h1> here, visually hidden but present in
  // the accessibility tree, restores one correct, view-specific heading per
  // route without adding a second <h1> on the routes that already have a
  // SectionHeader. It is derived from the same nav definition the sidebar
  // renders, so it can never name a view differently than the link that
  // leads to it. Prefix items (e.g. /jobs matching /jobs/:id) are checked
  // after exact/`end` matches so a nested route still resolves to its
  // parent's label.
  const location = useLocation();
  const navItems = groups.flatMap((g) => g.items);
  const currentItem =
    navItems.find((item) => location.pathname === item.to) ??
    navItems.find((item) => !item.end && location.pathname.startsWith(item.to)) ??
    navItems[0];

  return (
    <FlashProvider>
      <div className={styles.app}>
        <TopBar />
        <div className={styles.body}>
          <SideNav groups={groups} />
          <main className={styles.main}>
            <h1 className={styles.visuallyHidden}>{currentItem.label}</h1>
            <Outlet />
          </main>
        </div>
        <StatusBar />
      </div>
    </FlashProvider>
  );
}
