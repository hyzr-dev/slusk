import { NavLink } from 'react-router-dom';
import styles from './SideNav.module.css';

export interface NavItem {
  to: string;
  label: string;
  end?: boolean;
  badge?: number;
  /** Colour the badge as a problem rather than a count. */
  alert?: boolean;
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

/**
 * The grouped sidebar. Badges are suppressed at zero rather than rendered as
 * "0" — a nav full of zeros reads as broken, and the absence of a number is
 * the same information.
 *
 * Keyboard hints (the digit next to each entry in the design mock) are
 * deliberately absent until the bindings exist; see issue #199.
 */
export default function SideNav({ groups }: { groups: NavGroup[] }) {
  return (
    <nav className={styles.nav}>
      {groups.map((group) => (
        <div key={group.label}>
          <div className={styles.group}>{group.label}</div>
          {group.items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? styles.itemActive : styles.item)}
            >
              <span className={styles.label}>{item.label}</span>
              {item.badge ? (
                <span
                  className={item.alert ? styles.badgeAlert : styles.badge}
                  data-alert={item.alert ? 'true' : undefined}
                >
                  {item.badge}
                </span>
              ) : null}
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  );
}
