import type { ReactNode } from 'react';
import styles from './Panel.module.css';

/**
 * The free-standing card the 27 July restyle introduced
 * (docs/design/slusk-tui.dc.html, commit 688d52c): just the box —
 * border and `--panel` surface. It composes with `SectionHeader`, which
 * still owns the rule-and-label bar inside a panel; Panel does not
 * duplicate any of that, it only supplies the border content sits inside.
 * A panel with no internal header (e.g. the Jobs grid, which has its own
 * column header row instead) simply renders content directly.
 */
export default function Panel({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={className ? `${styles.panel} ${className}` : styles.panel}>{children}</div>;
}
