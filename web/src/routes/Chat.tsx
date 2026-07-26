import { t } from '../strings';
import styles from './Placeholder.module.css';

/**
 * Placeholder until issue #183 builds an HTTP surface for private messages.
 * The view exists now so the nav has its final shape and the keyboard bindings
 * in #199 do not have to be renumbered when it fills in.
 */
export default function Chat() {
  return (
    <div className={styles.wrap}>
      <div className={styles.title}>{t.placeholder.chatTitle}</div>
      <div className={styles.body}>{t.placeholder.chatBody}</div>
      <div className={styles.issue}>{t.placeholder.chatIssue}</div>
    </div>
  );
}
