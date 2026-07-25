import { t } from '../strings';
import styles from './Placeholder.module.css';

/**
 * Placeholder until issue #58 builds a search endpoint. The view exists now
 * so the nav has its final shape and the keyboard bindings in #199 do not
 * have to be renumbered when it fills in.
 */
export default function Search() {
  return (
    <div className={styles.wrap}>
      <div className={styles.title}>{t.placeholder.searchTitle}</div>
      <div className={styles.body}>{t.placeholder.searchBody}</div>
      <div className={styles.issue}>{t.placeholder.searchIssue}</div>
    </div>
  );
}
