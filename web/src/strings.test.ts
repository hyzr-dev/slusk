import { describe, expect, it } from 'vitest';
import { t, eventLabel, candidateStateLabel, stateLabel } from './strings';

describe('eventLabel', () => {
  it('returns the translated label for a known code', () => {
    expect(eventLabel('search_fallback')).toBe(t.event.search_fallback);
  });

  it('falls back to the raw code for an unrecognised code', () => {
    expect(eventLabel('some_future_event')).toBe('some_future_event');
  });

  // Asserted against the literal copy rather than t.event.search_excluded: the
  // raw-code fallback means a missing entry degrades silently, so comparing the
  // map to itself would still pass with the label gone (issue #319).
  it('has a human label for the server-exclusion event', () => {
    expect(eventLabel('search_excluded')).toBe('Search excluded by server');
  });
});

describe('candidateStateLabel', () => {
  it('returns the translated label for a known state', () => {
    expect(candidateStateLabel('SUCCEEDED')).toBe(t.candidateState.SUCCEEDED);
  });

  it('falls back to the raw state for an unrecognised state', () => {
    expect(candidateStateLabel('UNKNOWN_STATE')).toBe('UNKNOWN_STATE');
  });
});

describe('stateLabel', () => {
  it('returns the translated state when the state is known', () => {
    expect(stateLabel('DOWNLOADING', 'active')).toBe(t.state.DOWNLOADING);
  });

  it('falls back to the translated status when the state is unknown but the status is known', () => {
    expect(stateLabel('SOME_NEW_STATE', 'stalled')).toBe(t.status.stalled);
  });

  it('falls back to the raw status when neither the state nor the status is known', () => {
    expect(stateLabel('SOME_NEW_STATE', 'some_future_status')).toBe('some_future_status');
  });
});
