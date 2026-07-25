import { describe, expect, it } from 'vitest';
import { t } from '../../strings';
import { elapsedLabel } from './TopBar';

// Pure-function test, not wall-clock dependent: both arguments are plain
// numbers supplied by the test, never Date.now().
describe('elapsedLabel', () => {
  it('reports "now" before the first successful fetch (dataUpdatedAt is 0)', () => {
    expect(elapsedLabel(0, 1_000_000)).toBe(t.chrome.updatedNow);
  });

  it('reports "now" for sub-second elapsed time', () => {
    expect(elapsedLabel(1_000_000, 1_000_400)).toBe(t.chrome.updatedNow);
  });

  it('counts whole elapsed seconds once at least one has passed', () => {
    expect(elapsedLabel(1_000_000, 1_004_200)).toBe(t.chrome.updatedAgo(4));
  });

  it('keeps climbing for a long-stalled poll', () => {
    expect(elapsedLabel(1_000_000, 1_090_000)).toBe(t.chrome.updatedAgo(90));
  });
});
