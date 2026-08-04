import { useEffect, useRef } from 'react';
import Button from '../components/tui/Button';
import { t } from '../strings';
import styles from './BulkRetryDialog.module.css';

interface Props {
  /** Which of the two retryable filters the list is showing. */
  filter: 'failed' | 'parked';
  /**
   * The facet count already rendered on the chip. For `parked` this is the
   * retryable set exactly; for `failed` it is an upper bound — see the copy in
   * strings.ts and store.BulkRetryJobs.
   */
  count: number;
  pending: boolean;
  onConfirm: () => void;
  onClose: () => void;
}

function focusable(container: HTMLElement): HTMLElement[] {
  return Array.from(container.querySelectorAll<HTMLElement>('button:not([disabled])'));
}

/**
 * Confirmation for the jobs list's bulk retry (issue #378). Second modal in
 * this codebase; it follows IdentifyModal's scrim/panel/focus contract rather
 * than inventing a second one, and stays deliberately smaller — two buttons
 * and a sentence, no scrolling body.
 */
export default function BulkRetryDialog({ filter, count, pending, onConfirm, onClose }: Props) {
  const panelRef = useRef<HTMLDivElement>(null);
  const returnFocusRef = useRef<HTMLElement | null>(null);
  const scrimMouseDownOnBackground = useRef(false);

  useEffect(() => {
    returnFocusRef.current = document.activeElement as HTMLElement | null;
    // Confirm is the last focusable element in the panel; focusing it by
    // position rather than by ref keeps both buttons on the shared Button
    // component, which forwards no ref.
    const initial = focusable(panelRef.current!);
    initial[initial.length - 1]?.focus();
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose();
        return;
      }
      if (e.key === 'Tab' && panelRef.current) {
        const els = focusable(panelRef.current);
        if (els.length === 0) return;
        const first = els[0];
        const last = els[els.length - 1];
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }
    }
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
      returnFocusRef.current?.focus();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div
      className={styles.scrim}
      onMouseDown={(e) => {
        scrimMouseDownOnBackground.current = e.target === e.currentTarget;
      }}
      onClick={() => {
        if (scrimMouseDownOnBackground.current) onClose();
      }}
    >
      <div
        ref={panelRef}
        className={styles.panel}
        role="dialog"
        aria-modal="true"
        aria-label={t.jobs.bulkRetry.dialogLabel}
        onClick={(e) => e.stopPropagation()}
      >
        <div className={styles.header}>{t.jobs.bulkRetry.dialogTitle}</div>
        <div className={styles.body}>
          <p className={styles.line}>
            {filter === 'parked'
              ? t.jobs.bulkRetry.parkedBody(count)
              : t.jobs.bulkRetry.failedBody(count)}
          </p>
          <p className={styles.note}>{t.jobs.bulkRetry.lidarrNote}</p>
        </div>
        <div className={styles.footer}>
          <Button variant="ghost" onClick={onClose}>
            {t.jobs.bulkRetry.cancel}
          </Button>
          <Button variant="primary" disabled={pending} onClick={onConfirm}>
            {t.jobs.bulkRetry.confirm}
          </Button>
        </div>
      </div>
    </div>
  );
}
