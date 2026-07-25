import styles from './StatCard.module.css';

interface Props {
  label: string;
  value: number | string;
  // Optional colour dot next to the label and a muted sub-label under the
  // value (mock: docs/design/slskdarr-dashboard.dc.html lines 127-139).
  // Neither is shown for the plain two-field cards still used by Shares.
  dotColor?: string;
  sub?: string;
}

export default function StatCard({ label, value, dotColor, sub }: Props) {
  return (
    <div className={styles.card}>
      <div className={styles.labelRow}>
        {dotColor && <span className={styles.dot} style={{ background: dotColor }} aria-hidden="true" />}
        <span className={styles.label}>{label}</span>
      </div>
      <div className={styles.value}>{value}</div>
      {sub && <div className={styles.sub}>{sub}</div>}
    </div>
  );
}
