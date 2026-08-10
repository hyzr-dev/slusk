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

  // Issue #59: the backend writes this event from two different sites (an
  // unidentified download, or an identified one whose release group isn't in
  // Lidarr's library). Without an entry here the Events page and JobDetail
  // would render the raw snake_case code via the fallback above.
  it('has a human label for the not-imported event', () => {
    expect(eventLabel('not_imported')).toBe(t.event.not_imported);
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

  // Issue #59: NOT_IMPORTED is a real terminal state, not an unrecognised
  // one — it must resolve through t.state, not the status/raw fallbacks.
  it('returns the translated state for NOT_IMPORTED', () => {
    expect(stateLabel('NOT_IMPORTED', 'notImported')).toBe(t.state.NOT_IMPORTED);
  });

  // Issue #470: IMPORT_REFUSED is likewise a real terminal state.
  it('returns the translated state for IMPORT_REFUSED', () => {
    expect(stateLabel('IMPORT_REFUSED', 'importRefused')).toBe(t.state.IMPORT_REFUSED);
  });
});
