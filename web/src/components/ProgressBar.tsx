import { percent } from '../format';
import styles from './ProgressBar.module.css';

export default function ProgressBar({ done, total }: { done: number; total: number }) {
  return (
    <div className={styles.bar}>
      <div className={styles.fill} style={{ width: `${percent(done, total)}%` }} />
    </div>
  );
}
