import { describe, expect, it } from 'vitest';
import { parseGoDuration } from './goDuration';

describe('parseGoDuration', () => {
  it('parses minutes with a trailing zero-seconds component', () => {
    expect(parseGoDuration('15m0s')).toBe(900);
  });

  it('parses combined hours, minutes and seconds', () => {
    expect(parseGoDuration('1h30m0s')).toBe(5400);
  });

  it('parses seconds alone', () => {
    expect(parseGoDuration('45s')).toBe(45);
  });

  it('parses a bare hour value', () => {
    expect(parseGoDuration('2h')).toBe(7200);
  });

  it('parses sub-second units without being confused by the leading "m"/"s" letters', () => {
    expect(parseGoDuration('500ms')).toBeCloseTo(0.5);
    expect(parseGoDuration('10us')).toBeCloseTo(0.00001);
    expect(parseGoDuration('10ns')).toBeCloseTo(0.00000001);
  });

  it('parses fractional seconds and milliseconds', () => {
    expect(parseGoDuration('1.5s')).toBeCloseTo(1.5);
    expect(parseGoDuration('1.5ms')).toBeCloseTo(0.0015);
  });

  it('parses zero seconds as 0, not null', () => {
    // Distinct from the empty-string/garbage cases below: "0s" is a valid Go
    // duration, and callers (e.g. Header's reconcile countdown) round-trip a
    // zero result into "due now" rather than hiding the badge entirely.
    expect(parseGoDuration('0s')).toBe(0);
  });

  it('parses the micro sign (µ, U+00B5) Go actually emits, not just the ASCII "us" spelling', () => {
    expect(parseGoDuration('500µs')).toBeCloseTo(0.0005);
  });

  it('returns null for a negative duration', () => {
    // time.Duration.String() never emits a leading "-" the parser handles —
    // Go durations used here (intervals, deadlines) are never negative — so
    // this correctly falls through to the "unconsumed input" rejection: the
    // regex matches "1h0m0s" starting after the sign, leaving the leading
    // "-" unconsumed.
    expect(parseGoDuration('-1h0m0s')).toBeNull();
  });

  it('returns null for an empty string', () => {
    expect(parseGoDuration('')).toBeNull();
  });

  it('returns null for null and undefined', () => {
    expect(parseGoDuration(null)).toBeNull();
    expect(parseGoDuration(undefined)).toBeNull();
  });

  it('returns null for garbage input', () => {
    expect(parseGoDuration('not a duration')).toBeNull();
    expect(parseGoDuration('15minutes')).toBeNull();
    expect(parseGoDuration('m15s')).toBeNull();
  });

  it('accepts a repeated unit as their sum (e.g. "5s5s" -> 10s) — harmless since Go never emits this, not worth rejecting', () => {
    expect(parseGoDuration('5s5s')).toBe(10);
  });
});
