import type { JobState } from '../api/types';
import { t } from '../strings';

interface Props {
  state: JobState | undefined;
  className?: string;
}

export default function ParkedExplanation({ state, className }: Props) {
  if (state !== 'PARKED') return null;
  return <div className={className}>{t.jobs.parkedExplanation}</div>;
}
