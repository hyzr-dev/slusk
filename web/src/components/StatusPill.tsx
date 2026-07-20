import type { JobState, JobStatus } from '../api/types';
import { stateLabel } from '../strings';
import styles from './StatusPill.module.css';

interface Props {
  status: JobStatus;
  state: JobState;
}

// An unknown state degrades to the coarser status label rather than showing a
// raw enum string — this two-level fallback is inherited from the legacy
// dashboard and is deliberate. See strings.ts `stateLabel` for the lookup.
export default function StatusPill({ status, state }: Props) {
  const label = stateLabel(state, status);
  return <span className={`${styles.pill} ${styles[status] ?? ''}`}>{label}</span>;
}
