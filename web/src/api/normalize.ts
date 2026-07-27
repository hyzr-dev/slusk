import type {
  Job,
  JobDetail,
  JobPage,
  JobState,
  JobStatus,
  StatusReport,
  WireJob,
  WireJobDetail,
  WireJobPage,
  WireJobState,
  WireJobStatus,
  WireStatusReport,
} from './types';

function normalizeJobStatus(status: WireJobStatus): JobStatus {
  return status === 'orphaned' ? 'parked' : status;
}

function normalizeJobState(state: WireJobState): JobState {
  return state === 'ORPHANED' ? 'PARKED' : state;
}

export function normalizeJob(job: WireJob): Job {
  return {
    ...job,
    status: normalizeJobStatus(job.status),
    state: normalizeJobState(job.state),
  };
}

export function normalizeJobs(jobs: WireJob[]): Job[] {
  return jobs.map(normalizeJob);
}

export function normalizeJobPage(page: WireJobPage): JobPage {
  return { ...page, jobs: normalizeJobs(page.jobs) };
}

export function normalizeJobDetail(detail: WireJobDetail): JobDetail {
  return { ...detail, state: normalizeJobState(detail.state) };
}

export function normalizeStatusReport(report: WireStatusReport): StatusReport {
  return {
    queued: report.queued,
    active: report.active,
    stalled: report.stalled,
    parked: report.parked ?? report.orphaned ?? 0,
    modules: report.modules,
    moduleDetails: report.moduleDetails,
    ...(report.version === undefined ? {} : { version: report.version }),
  };
}
