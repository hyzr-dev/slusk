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
 */
export default function SectionHeader({ label, meta }: { label: string; meta?: ReactNode }) {
  return (
    <div className={styles.header}>
      <h2 className={styles.label}>{label}</h2>
      <span className={styles.spacer} />
      {meta ? <span className={styles.meta}>{meta}</span> : null}
    </div>
  );
}
