import styles from './StatCard.module.css';

export default function StatCard({ label, value }: { label: string; value: number | string }) {
  return (
    <div className={styles.card}>
      <div className={styles.label}>{label}</div>
      <div className={styles.value}>{value}</div>
    </div>
  );
}
