import { NavLink, Outlet } from 'react-router-dom';
import { t } from '../strings';
import styles from './Layout.module.css';

const NAV = [
  { to: '/', label: t.nav.overview, end: true },
  { to: '/jobs', label: t.nav.jobs, end: false },
  { to: '/events', label: t.nav.events, end: false },
  { to: '/peers', label: t.nav.peers, end: false },
  { to: '/shares', label: t.nav.shares, end: false },
  { to: '/health', label: t.nav.health, end: false },
  { to: '/settings', label: t.nav.settings, end: false },
];

export default function Layout() {
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
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? styles.navItemActive : styles.navItem)}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  );
}
