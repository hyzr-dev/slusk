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
});
