import type { JobSource, JobState } from '../api/types';
import { t } from '../strings';

interface Props {
  state: JobState | undefined;
  // A manual job is offered a different set of actions (JobActions gates
  // 'Re-run pipeline' on source !== 'manual'), and its Retry does something
  // else entirely, so the action copy cannot be one string. Absent is
  // treated as 'lidarr', matching JobActions' own default.
  source?: JobSource;
  // The job's own job_parked event detail (issue #484). Absent for jobs
  // parked before that event existed — falls back to the static, cause-free
  // lead sentence.
  detail?: string;
  className?: string;
}

export default function ParkedExplanation({ state, source, detail, className }: Props) {
  if (state !== 'PARKED') return null;
  const lead = detail ? t.jobs.parkedReason(detail) : t.jobs.parkedLead;
  const actions = source === 'manual' ? t.jobs.parkedActionsManual : t.jobs.parkedActions;
  return (
    <div className={className}>
      {lead} {actions}
    </div>
  );
}
