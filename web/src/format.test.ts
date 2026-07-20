import { describe, expect, it } from 'vitest';
import { formatBytes, percent, formatDateTime, formatShortTime, formatScore } from './format';

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
  it('formats a full timestamp in sv-SE', () => {
    expect(formatDateTime('2026-07-20T14:32:05Z')).toMatch(/2026-07-20/);
  });

  it('formats short time as HH:MM', () => {
    expect(formatShortTime('2026-07-20T14:32:05Z')).toMatch(/^\d{2}:\d{2}$/);
  });

  it('returns an em dash for empty input rather than "Invalid Date"', () => {
    expect(formatDateTime('')).toBe('—');
    expect(formatShortTime('')).toBe('—');
  });
});

describe('formatScore', () => {
  it('always shows two decimals', () => {
    expect(formatScore(1)).toBe('1.00');
    expect(formatScore(0.456)).toBe('0.46');
  });
});
