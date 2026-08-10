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

  // Issue #472: the backend writes job_parked from three sites (two in
  // Downloading, one in Importing). Asserted against the literal copy rather
  // than t.event.job_parked, for the reason given above — the raw-code
  // fallback means a missing entry degrades silently into 'job_parked' on the
  // timeline, and comparing the map to itself would not notice.
  it('has a human label for the job-parked event', () => {
    expect(eventLabel('job_parked')).toBe('Job parked');
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

// The PARKED explanation is shown on both the jobs list and the job detail
// panel, and three backend paths now lead there: two in Downloading (a
// transfer that kept vanishing, a cancel the backend would not confirm) and
// one in Importing (issue #472, repeated identical candidate rejections).
// Copy that names any one of those causes is a fabricated diagnosis for the
// other two.
describe('parkedExplanation', () => {
  // An allowlist, not a blocklist. Listing the phrases previous versions got
  // wrong only ever catches those exact phrases: the copy was already once
  // rewritten from one wrong cause ('backend disappearance') straight into
  // another ('no candidate could satisfy this album'), and a blocklist of the
  // first reports success on the second. Causal verbs are the thing being
  // banned, so ban the grammar.
  //
  // This ban applies only to parkedLead, the static fallback shown when the
  // job carries no parkDetail (issue #484). parkedReason interpolates the
  // backend's own job_parked event text — which is free to name any of the
  // three causes, because it is reporting the one that actually happened for
  // *this* job, not guessing at one of three — so it is deliberately exempt.
  const causalClaim = /because|repeated|exhaust|disappear|no candidate|offline|unavailable|could not (be )?(find|found)/i;

  it('states no cause in the static fallback lead, because the backend has three', () => {
    expect(t.jobs.parkedLead).not.toMatch(causalClaim);
  });

  it('points at the job\'s own events, which do know the cause', () => {
    expect(t.jobs.parkedLead).toMatch(/events/i);
  });

  it('renders a backend-supplied reason as a labelled, punctuated sentence', () => {
    expect(t.jobs.parkedReason('no candidate could satisfy this album: 5 rejected')).toBe(
      'Parked — no candidate could satisfy this album: 5 rejected.',
    );
  });

  // Issue #487. The pointer above is right; naming *where* it points was not.
  // ParkedExplanation has two hosts and only JobDetail puts an EVENTS section
  // under it - JobExpansion renders a meta tree and a file list - so 'the
  // events below' was false for every parked job read from the jobs list, and
  // sent that reader hunting through filenames.
  //
  // Banned as grammar rather than as the one phrase that shipped, for the
  // reason causalClaim gives: 'the timeline underneath' would pass a blocklist
  // of 'below' while being just as false. A locator is only ever correct on
  // one of the two surfaces, so no locator can be correct here.
  //
  // jsdom cannot catch this class of defect - it renders both hosts happily
  // without ever relating a sentence's promise to another file's JSX - so the
  // guard has to live on the string.
  const positionalLocator = /\b(below|above|beneath|underneath|further down|down the page)\b/i;

  // #484 split the two blobs into lead + actions, so the pointer this guards
  // now lives in exactly one string. Asserted on the lead alone rather than
  // dropped: it is the only string that still names the events.
  it('does not tell the reader where the events are, having two hosts', () => {
    expect(t.jobs.parkedLead).not.toMatch(positionalLocator);
  });

  // The two buttons carry different budgets and nothing else on screen shows
  // it, so the Lidarr copy names them - by their rendered labels. #376
  // renamed 'Force search' to 'Re-run pipeline', and copy naming a button
  // that is not on screen misdirects rather than helps.
  it('names the Lidarr buttons exactly as they are rendered', () => {
    expect(t.jobs.parkedActions).toContain(t.jobs.retry);
    expect(t.jobs.parkedActions).toContain(t.jobs.forceSearch);
  });

  // JobActions gates 'Re-run pipeline' on source !== 'manual', so naming it
  // in the manual copy sends the user hunting for a button that is not there.
  it('does not offer a manual job the button it cannot have', () => {
    expect(t.jobs.parkedActionsManual).not.toContain(t.jobs.forceSearch);
    expect(t.jobs.parkedActionsManual).toContain(t.jobs.retry);
  });
});
