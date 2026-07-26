import { describe, expect, it } from 'vitest';
import { t } from '../../strings';
import { stalenessLabel } from './TopBar';

const STALE_AFTER = 10_000;

// Pure-function test, not wall-clock dependent: every argument is a plain
// number supplied by the test, never Date.now().
describe('stalenessLabel', () => {
  it('reports nothing before the first successful fetch (dataUpdatedAt is 0)', () => {
    // That state is "no news yet", not evidence that polling has stopped.
    expect(stalenessLabel(0, 1_000_000, STALE_AFTER)).toBeNull();
  });

  it('stays silent while the poll is keeping up', () => {
    // The whole point of the redesign: during normal operation the cell shows
    // no digits at all, so a number appearing there always means trouble.
    expect(stalenessLabel(1_000_000, 1_000_400, STALE_AFTER)).toBeNull();
    expect(stalenessLabel(1_000_000, 1_004_200, STALE_AFTER)).toBeNull();
    expect(stalenessLabel(1_000_000, 1_009_999, STALE_AFTER)).toBeNull();
  });

  it('speaks up once two polls in a row have been missed', () => {
    expect(stalenessLabel(1_000_000, 1_010_000, STALE_AFTER)).toBe(t.chrome.stale('10s'));
  });

  it('coarsens the age past a minute rather than counting seconds forever', () => {
    expect(stalenessLabel(1_000_000, 1_090_000, STALE_AFTER)).toBe(t.chrome.stale('1m'));
    expect(stalenessLabel(1_000, 3_601_000, STALE_AFTER)).toBe(t.chrome.stale('1h'));
  });
});
