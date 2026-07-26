import type { ReactNode } from 'react';
import styles from './SectionHeader.module.css';

/**
 * The rule that opens every panel: an all-caps label on the left and quiet
 * meta on the right. `label` is expected to arrive already upper-cased from
 * strings.ts rather than transformed here, so a translation can opt out of
 * casing that does not survive in its script.
 *
 * The label renders as an <h2> — every use of this component is the heading
 * for the panel content that follows it — so the document retains a heading
 * outline for screen-reader navigation even though the mock it's styled
 * after (a terminal UI) has no notion of headings at all. `.label` resets
 * the browser's default heading font/margin back to `.header`'s own
 * styling, so this is visually identical to a plain <span>.
 *
 * Set `prominent` when the header names the page's subject rather than
 * labelling a panel — a job's album and artist on the detail page, say. The
 * quiet default works because a section label sits above content that
 * carries the meaning; a page subject *is* the meaning and has to be read.
 */
export default function SectionHeader({
  label,
  meta,
  prominent = false,
}: {
  label: string;
  meta?: ReactNode;
  prominent?: boolean;
}) {
  return (
    <div className={prominent ? `${styles.header} ${styles.prominent}` : styles.header}>
      <h2 className={styles.label}>{label}</h2>
      <span className={styles.spacer} />
      {meta ? <span className={styles.meta}>{meta}</span> : null}
    </div>
  );
}
