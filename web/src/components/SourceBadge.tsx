import type { JobSource } from '../api/types';
import { t } from '../strings';
import styles from './SourceBadge.module.css';

// Source is an axis orthogonal to status, so it deliberately never borrows a
// status color: "manual" gets the one distinct cyan hue in the palette,
// "lidarr" is neutral grey (see tokens.css --manual/--manual-bg).
export default function SourceBadge({ source }: { source: JobSource }) {
  const label = source === 'manual' ? t.source.manual : t.source.lidarr;
  return (
    <span className={`${styles.badge} ${source === 'manual' ? styles.manual : styles.lidarr}`}>
      {label}
    </span>
  );
}
