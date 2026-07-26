import type { ReactNode } from 'react';
import styles from './SectionHeader.module.css';

/**
 * The rule that opens every panel: a label on the left and quiet meta on the
 * right. Casing lives in strings.ts rather than a `text-transform` here, so a
 * translation can opt out of casing that does not survive in its script —
 * `label` renders exactly as it arrives.
 *
 * Static panel labels are therefore upper-cased at source; a dynamic subject —
 * a job's album title on the detail page — obviously cannot be, and reads
 * as-is.
 *
 * `prominent` is a separate axis: it makes the header loud, for when it names
 * the page's subject rather than labelling a panel. The quiet default works
 * because a section label sits above content that carries the meaning; a page
 * subject *is* the meaning and has to be read.
 *
 * The label renders as an <h2> — every use of this component is the heading
 * for the panel content that follows it — so the document retains a heading
 * outline for screen-reader navigation even though the mock it's styled
 * after (a terminal UI) has no notion of headings at all. `.label` resets
 * the browser's default heading font/margin back to `.header`'s own
 * styling, so this is visually identical to a plain <span>.
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
