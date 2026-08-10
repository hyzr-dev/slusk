import type { JobSource, JobState } from '../api/types';
import { t } from '../strings';

interface Props {
  state: JobState | undefined;
  // A manual job is offered a different set of actions (JobActions gates
  // 'Re-run pipeline' on source !== 'manual'), and its Retry does something
  // else entirely, so the copy cannot be one string. Absent is treated as
  // 'lidarr', matching JobActions' own default.
  source?: JobSource;
  className?: string;
}

export default function ParkedExplanation({ state, source, className }: Props) {
  if (state !== 'PARKED') return null;
  const copy = source === 'manual' ? t.jobs.parkedExplanationManual : t.jobs.parkedExplanation;
  return <div className={className}>{copy}</div>;
}
