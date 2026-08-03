import type { CSSProperties, ReactNode } from 'react';
import styles from './Page.module.css';

/**
 * The per-view shell introduced by the 27 July TUI restyle
 * (docs/design/slusk-tui.dc.html, commit 688d52c): every route now owns
 * its own padded, gap-separated column instead of rendering flush against
 * `<main>`, which keeps `padding: 0` (Layout.module.css) for exactly this
 * reason — double-padding would shift every panel a second time.
 *
 * `title` renders as the route's only `<h1>`, replacing the visually-hidden
 * one Layout used to synthesize from the nav entry (see Layout.tsx) — now
 * that every route has its own visible title, deriving a second, invisible
 * one from the matching nav item would just duplicate it.
 *
 * `maxWidth` overrides the mock's default 1320px column for the two views
 * that read narrower in the mock (Setup: 820px, Config: 900px).
 */
export default function Page({
  title,
  subtitle,
  maxWidth,
  children,
}: {
  title: string;
  subtitle?: string;
  maxWidth?: number;
  children: ReactNode;
}) {
  const style: CSSProperties | undefined = maxWidth ? { maxWidth } : undefined;
  return (
    <div className={styles.page} style={style}>
      <div className={styles.head}>
        <div>
          <h1 className={styles.title}>{title}</h1>
          {subtitle && <div className={styles.subtitle}>{subtitle}</div>}
        </div>
      </div>
      {children}
    </div>
  );
}
