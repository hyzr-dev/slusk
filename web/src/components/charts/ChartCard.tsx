import type { ReactNode } from 'react';
import styles from './ChartCard.module.css';

export default function ChartCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className={styles.card}>
      <div className={styles.title}>{title}</div>
      {children}
    </div>
  );
}
