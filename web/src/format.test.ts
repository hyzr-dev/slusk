import { describe, expect, it } from 'vitest';
import {
  basename,
  formatBytes,
  formatBytesOrDash,
  formatSpeed,
  formatEta,
  formatSize,
  percent,
  formatDateTime,
  formatShortTime,
  formatTime,
  formatScore,
} from './format';

describe('formatBytes', () => {
  it('returns "0 MB" for zero and nullish input', () => {
    expect(formatBytes(0)).toBe('0 MB');
    expect(formatBytes(null)).toBe('0 MB');
    expect(formatBytes(undefined)).toBe('0 MB');
  });

  it('formats megabytes with one decimal', () => {
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    expect(formatBytes(1536 * 1024)).toBe('1.5 MB');
  });

  // Matches the legacy dashboard: everything is expressed in MB, never scaled.
  it('does not scale to GB', () => {
    expect(formatBytes(5 * 1024 * 1024 * 1024)).toBe('5120.0 MB');
  });
});

describe('formatSpeed', () => {
  it('returns an em dash for zero and nullish input, never "0 B/s"', () => {
    expect(formatSpeed(0)).toBe('—');
    expect(formatSpeed(null)).toBe('—');
    expect(formatSpeed(undefined)).toBe('—');
  });

  it('scales to KB/s below 1 MB/s', () => {
    expect(formatSpeed(524288)).toBe('512 KB/s');
  });

  it('scales to MB/s at or above 1 MB/s', () => {
    expect(formatSpeed(1024 * 1024)).toBe('1.0 MB/s');
    expect(formatSpeed(1536 * 1024)).toBe('1.5 MB/s');
  });
});

describe('formatEta', () => {
  it('returns an em dash for zero and nullish input', () => {
    expect(formatEta(0)).toBe('—');
    expect(formatEta(null)).toBe('—');
    expect(formatEta(undefined)).toBe('—');
  });

  it('shows seconds below a minute', () => {
    expect(formatEta(45)).toBe('45 s');
  });

  it('shows rounded minutes at or above 60 seconds', () => {
    expect(formatEta(60)).toBe('1 min');
    expect(formatEta(150)).toBe('3 min');
  });

  it('shows hours and minutes at or above one hour', () => {
    expect(formatEta(3600)).toBe('1 h');
    expect(formatEta(5400)).toBe('1 h 30 min');
    expect(formatEta(7260)).toBe('2 h 1 min');
  });

  it('carries the rounded remainder instead of reporting 60 minutes', () => {
    expect(formatEta(7199)).toBe('2 h');
    expect(formatEta(86399)).toBe('24 h');
  });
});

describe('formatBytesOrDash', () => {
  it('returns an em dash for zero and nullish input, unlike formatBytes', () => {
    expect(formatBytesOrDash(0)).toBe('—');
    expect(formatBytesOrDash(null)).toBe('—');
    expect(formatBytesOrDash(undefined)).toBe('—');
  });

  it('formats non-zero sizes the same as formatBytes', () => {
    expect(formatBytesOrDash(1024 * 1024)).toBe('1.0 MB');
  });
});

describe('basename', () => {
  it('returns the leaf name for a forward-slash path', () => {
    expect(basename('music/Album/01 Track.flac')).toBe('01 Track.flac');
  });

  it('returns the leaf name for a backslash path', () => {
    expect(basename('music\\Album\\01 Track.flac')).toBe('01 Track.flac');
  });

  it('returns the input unchanged when there is no separator', () => {
    expect(basename('01 Track.flac')).toBe('01 Track.flac');
  });
});

describe('formatSize', () => {
  it('returns "0 MB" for zero and nullish input', () => {
    expect(formatSize(0)).toBe('0 MB');
    expect(formatSize(null)).toBe('0 MB');
    expect(formatSize(undefined)).toBe('0 MB');
  });

  it('formats megabytes with one decimal below 1 GB', () => {
    expect(formatSize(1536 * 1024)).toBe('1.5 MB');
  });

  it('scales to GB at or above 1024 MB', () => {
    expect(formatSize(742 * 1024 * 1024 * 1024)).toBe('742.0 GB');
  });

  it('scales to TB at or above 1024 GB', () => {
    expect(formatSize(1.4 * 1024 * 1024 * 1024 * 1024)).toBe('1.4 TB');
  });
});

describe('percent', () => {
  it('returns 0 when total is zero', () => {
    expect(percent(100, 0)).toBe(0);
  });

  it('rounds to nearest integer', () => {
    expect(percent(1, 3)).toBe(33);
    expect(percent(2, 3)).toBe(67);
  });

  // Deliberate change from the legacy dashboard, which let the bar overflow.
  it('clamps to 100', () => {
    expect(percent(150, 100)).toBe(100);
  });

  it('clamps negatives to 0', () => {
    expect(percent(-5, 100)).toBe(0);
  });
});

describe('date formatting', () => {
  // sv-SE is kept deliberately: ISO-like dates read better in a technical tool.
  // The exact wall-clock date/time depends on the runner's TZ, so these assert
  // shape only — asserting a literal date broke under TZ=Pacific/Auckland
  // (UTC+12/13), where 2026-07-20T14:32:05Z rolls over to the 21st locally.
  it('formats a full timestamp in sv-SE shape', () => {
    expect(formatDateTime('2026-07-20T14:32:05Z')).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/);
  });

  it('formats short time as HH:MM', () => {
    expect(formatShortTime('2026-07-20T14:32:05Z')).toMatch(/^\d{2}:\d{2}$/);
  });

  it('returns an em dash for empty input rather than "Invalid Date"', () => {
    expect(formatDateTime('')).toBe('—');
    expect(formatShortTime('')).toBe('—');
  });
});

describe('formatTime', () => {
  it('returns an em dash for empty input', () => {
    expect(formatTime('')).toBe('—');
  });

  // Shape only, for the same TZ-rollover reason as formatDateTime above.
  it('formats time as HH:MM:SS', () => {
    expect(formatTime('2026-07-20T14:32:05Z')).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });
});

describe('formatScore', () => {
  it('always shows two decimals', () => {
    expect(formatScore(1)).toBe('1.00');
    expect(formatScore(0.456)).toBe('0.46');
  });
});
