import type { ReactNode } from 'react';
import styles from './SectionHeader.module.css';

/**
 * The rule that opens every panel: an all-caps label on the left and quiet
 * meta on the right. `label` is expected to arrive already upper-cased from
 * strings.ts rather than transformed here, so a translation can opt out of
 * casing that does not survive in its script.
 */
export default function SectionHeader({ label, meta }: { label: string; meta?: ReactNode }) {
  return (
    <div className={styles.header}>
      <span>{label}</span>
      <span className={styles.spacer} />
      {meta ? <span className={styles.meta}>{meta}</span> : null}
    </div>
  );
}
