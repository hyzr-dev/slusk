import styles from './EmptyState.module.css';

/**
 * The `── NOTHING HERE ──` rule. The dashes are added here rather than baked
 * into every string in strings.ts so the decoration stays a styling decision.
 */
export default function EmptyState({ message }: { message: string }) {
  return <div className={styles.empty}>{`── ${message} ──`}</div>;
}
