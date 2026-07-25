import styles from './Chip.module.css';

interface Props {
  label: string;
  count?: number;
  active?: boolean;
  onClick: () => void;
}

/**
 * A filter or sort toggle. `aria-pressed` rather than a radio group: the
 * chips in the jobs view are a single-select filter, but the sort chips in a
 * later view are not, and one component serving both keeps the visual
 * treatment identical.
 */
export default function Chip({ label, count, active = false, onClick }: Props) {
  return (
    <button
      type="button"
      aria-pressed={active}
      className={`${styles.chip} ${active ? styles.active : ''}`}
      onClick={onClick}
    >
      {label}
      {count === undefined ? null : (
        <span className={active ? styles.activeCount : styles.count}>{count}</span>
      )}
    </button>
  );
}
