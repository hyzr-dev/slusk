import type {
  Job,
  JobDetail,
  JobPage,
  JobState,
  JobStatus,
  SearchFile,
  SearchGroup,
  SearchSession,
  StatusReport,
  WireJob,
  WireJobDetail,
  WireJobPage,
  WireJobState,
  WireJobStatus,
  WireSearchFile,
  WireSearchGroup,
  WireSearchSession,
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
  return { ...detail, job: normalizeJob(detail.job) };
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

// Identity today (see SearchFile/SearchGroup/SearchSession's doc comment in
// types.ts) — kept as real functions, not type-only aliasing, so both
// transports that carry this shape (the POST/GET REST snapshot and the
// `event: search` SSE delta's per-group entries — see queries.ts's
// replaceSearchGroups) normalize through the same call, exactly once.
export function normalizeSearchFile(file: WireSearchFile): SearchFile {
  return file;
}

export function normalizeSearchGroup(group: WireSearchGroup): SearchGroup {
  return { ...group, files: group.files.map(normalizeSearchFile) };
}

export function normalizeSearchSession(session: WireSearchSession): SearchSession {
  return { ...session, groups: session.groups.map(normalizeSearchGroup) };
}

/**
 * The bitrate badge for a search result card. A VBR file's nominal bitrate
 * (its BitRate attribute, code 0) is meaningless on its own — an MP3 V0 file
 * reports roughly 245 kbps, which reads as "worse than a 320 kbps CBR file"
 * when the opposite is usually true — so a group flagged variableBitRate
 * renders "VBR" instead of a number. Returns undefined (render nothing) when
 * the peer sent no bitrate attribute at all.
 */
export function formatBitrateLabel(group: Pick<SearchGroup, 'bitrate' | 'variableBitRate'>): string | undefined {
  if (group.variableBitRate) return 'VBR';
  if (group.bitrate === undefined) return undefined;
  return `${group.bitrate} kbps`;
}

/**
 * The sample-rate/bit-depth badge ("16/44"), shown only when the peer
 * reported both attributes — a lone sample rate or bit depth is a partial
 * fact not worth a badge of its own (issue #58 §4).
 */
export function formatQualityLabel(group: Pick<SearchGroup, 'sampleRate' | 'bitDepth'>): string | undefined {
  if (group.sampleRate === undefined || group.bitDepth === undefined) return undefined;
  const khz = group.sampleRate % 1000 === 0 ? `${group.sampleRate / 1000}` : (group.sampleRate / 1000).toFixed(1);
  return `${group.bitDepth}/${khz}`;
}
