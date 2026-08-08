import { describe, expect, it } from 'vitest';
import type { WireJob, WireSearchGroup, WireSearchSession, WireStatusReport } from './types';
import {
  formatBitrateLabel,
  formatQualityLabel,
  normalizeJobDetail,
  normalizeJobPage,
  normalizeJobs,
  normalizeSearchSession,
  normalizeStatusReport,
} from './normalize';

function wireJob(overrides: Partial<WireJob> = {}): WireJob {
  return {
    id: 1,
    title: 'Kind of Blue',
    artist: 'Miles Davis',
    status: 'active',
    peer: '',
    bytesDone: 0,
    bytesTotal: 0,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    framedAt: '2026-01-01T00:00:00Z',
    state: 'DOWNLOADING',
    candidatesTried: 0,
    maxCandidates: 3,
    failReason: '',
    nextAttemptAt: '',
    retries: 0,
    notBefore: '',
    source: 'lidarr',
    year: null,
    tracks: null,
    format: null,
    ...overrides,
  };
}

function wireStatus(overrides: Partial<WireStatusReport> = {}): WireStatusReport {
  return {
    queued: 1,
    active: 2,
    stalled: 3,
    modules: {},
    moduleDetails: {},
    ...overrides,
  };
}

describe('wire compatibility normalization', () => {
  it('normalizes old job-list and detail payloads to canonical parked values', () => {
    const [job] = normalizeJobs([
      wireJob({ status: 'orphaned', state: 'ORPHANED' }),
    ]);
    const detail = normalizeJobDetail({
      job: wireJob({ status: 'orphaned', state: 'ORPHANED' }),
      attempts: [],
    });

    expect(job).toMatchObject({ status: 'parked', state: 'PARKED' });
    expect(detail.job).toMatchObject({ status: 'parked', state: 'PARKED' });
  });

  it('normalizes nested page jobs without losing total or facets', () => {
    const page = normalizeJobPage({
      jobs: [wireJob({ status: 'orphaned', state: 'ORPHANED' })],
      total: 25,
      facets: {
        status: { all: 25, wanted: 0, selecting: 0, queued: 0, active: 2, importing: 1, waiting: 3, stalled: 4, failed: 5, parked: 6, done: 4, notImported: 0 },
        source: { all: 25, manual: 5, lidarr: 20 },
      },
    });

    expect(page.jobs[0]).toMatchObject({ status: 'parked', state: 'PARKED' });
    expect(page.total).toBe(25);
    expect(page.facets.source).toEqual({ all: 25, manual: 5, lidarr: 20 });
  });

  it('accepts new and mixed job payloads without exposing a legacy UI value', () => {
    const jobs = normalizeJobs([
      wireJob({ id: 1, status: 'parked', state: 'PARKED' }),
      wireJob({ id: 2, status: 'parked', state: 'ORPHANED' }),
    ]);

    expect(jobs.map(({ status, state }) => ({ status, state }))).toEqual([
      { status: 'parked', state: 'PARKED' },
      { status: 'parked', state: 'PARKED' },
    ]);
  });

  it('supplies zero for the status counts an older server omits', () => {
    // wireStatus() carries no wanted/selecting/waiting, exactly like a binary
    // predating issue #417. The Health page renders these straight into its
    // metric rows, so undefined would reach the DOM as an empty cell.
    const normalized = normalizeStatusReport(wireStatus());

    expect(normalized.wanted).toBe(0);
    expect(normalized.selecting).toBe(0);
    expect(normalized.waiting).toBe(0);
  });

  it('keeps the new status counts a current server sends', () => {
    const normalized = normalizeStatusReport(
      wireStatus({ wanted: 143, selecting: 4, waiting: 3 }),
    );

    expect(normalized).toMatchObject({ wanted: 143, selecting: 4, waiting: 3 });
  });

  it('falls back to orphaned for an old status payload', () => {
    expect(normalizeStatusReport(wireStatus({ orphaned: 4 })).parked).toBe(4);
  });

  it('prefers parked when a mixed status payload contains differing counts', () => {
    const normalized = normalizeStatusReport(wireStatus({ parked: 5, orphaned: 99 }));

    expect(normalized.parked).toBe(5);
    expect(normalized).not.toHaveProperty('orphaned');
  });
});

function wireSearchGroup(overrides: Partial<WireSearchGroup> = {}): WireSearchGroup {
  return {
    id: 'a1b2c3',
    peer: 'lossless_lars',
    // The server normalizes `folder` to `/`-separated for display
    // (matcher.ReleaseDir) — unlike files[].filename below, which stays the
    // raw peer-syntax path.
    folder: '@@abc/Music/Radiohead/In Rainbows',
    title: 'In Rainbows',
    parent: 'Radiohead',
    trackCount: 10,
    sizeBytes: 432000000,
    freeUploadSlot: true,
    queueLength: 0,
    uploadSpeed: 940000,
    score: 0.91,
    files: [],
    ...overrides,
  };
}

describe('search normalization', () => {
  it('normalizes a session and every nested group and file, preserving optional attributes', () => {
    const wire: WireSearchSession = {
      id: 'deadbeef',
      query: 'in rainbows',
      startedAt: '2026-01-01T00:00:00Z',
      done: false,
      streaming: true,
      total: 1,
      groups: [
        wireSearchGroup({
          format: 'flac',
          bitrate: 1010,
          sampleRate: 44100,
          bitDepth: 16,
          files: [
            { filename: '@@abc\\Music\\Radiohead\\In Rainbows\\05 - Nude.flac', name: '05 - Nude.flac', size: 34112000, bitrate: 1010 },
          ],
        }),
      ],
    };

    const session = normalizeSearchSession(wire);

    expect(session.id).toBe('deadbeef');
    expect(session.groups[0].format).toBe('flac');
    expect(session.groups[0].files[0].filename).toBe('@@abc\\Music\\Radiohead\\In Rainbows\\05 - Nude.flac');
    // The full peer-syntax path must pass through untouched — POST /api/jobs
    // needs it verbatim, unlike `folder`, which the server already
    // normalizes to `/` for display.
    expect(session.groups[0].files[0].bitrate).toBe(1010);
  });

  it('leaves every omitted optional attribute absent, not zeroed', () => {
    const session = normalizeSearchSession({
      id: 'deadbeef',
      query: 'q',
      startedAt: '2026-01-01T00:00:00Z',
      done: true,
      streaming: false,
      total: 1,
      groups: [wireSearchGroup({ files: [{ filename: 'f.mp3', name: 'f.mp3', size: 1000 }] })],
    });

    expect(session.groups[0].format).toBeUndefined();
    expect(session.groups[0].bitrate).toBeUndefined();
    expect(session.groups[0].files[0].bitrate).toBeUndefined();
    expect(session.groups[0].files[0].durationSeconds).toBeUndefined();
  });
});

describe('formatBitrateLabel', () => {
  it('renders VBR instead of a number when the group is flagged variable', () => {
    expect(formatBitrateLabel({ bitrate: 245, variableBitRate: true })).toBe('VBR');
  });

  it('renders a fixed kbps figure for a CBR file', () => {
    expect(formatBitrateLabel({ bitrate: 320, variableBitRate: false })).toBe('320 kbps');
  });

  it('renders nothing when the peer reported no bitrate attribute at all', () => {
    expect(formatBitrateLabel({ variableBitRate: false })).toBeUndefined();
  });
});

describe('formatQualityLabel', () => {
  it('combines bit depth and sample rate in kHz', () => {
    expect(formatQualityLabel({ sampleRate: 44100, bitDepth: 16 })).toBe('16/44.1');
  });

  it('renders a whole-number kHz figure when the sample rate divides evenly', () => {
    expect(formatQualityLabel({ sampleRate: 48000, bitDepth: 24 })).toBe('24/48');
  });

  it('renders nothing when only one of the two attributes is present', () => {
    expect(formatQualityLabel({ sampleRate: 44100, bitDepth: undefined })).toBeUndefined();
    expect(formatQualityLabel({ sampleRate: undefined, bitDepth: 16 })).toBeUndefined();
  });
});
