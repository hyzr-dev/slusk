import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { t } from '../strings';
import ParkedExplanation from './ParkedExplanation';

// Issue #484: a parked job states its own reason (job_parked event detail)
// instead of the static cause-free fallback, when the backend supplied one.
describe('ParkedExplanation', () => {
  it('renders the static lead and Lidarr actions when no detail is supplied', () => {
    const { container } = render(<ParkedExplanation state="PARKED" source="lidarr" />);

    expect(container.textContent).toBe(`${t.jobs.parkedLead} ${t.jobs.parkedActions}`);
  });

  it('renders the static lead and manual actions when no detail is supplied', () => {
    const { container } = render(<ParkedExplanation state="PARKED" source="manual" />);

    expect(container.textContent).toBe(`${t.jobs.parkedLead} ${t.jobs.parkedActionsManual}`);
  });

  it('replaces the static lead with the job-specific reason for a Lidarr job when a detail is supplied', () => {
    const detail = 'no candidate could satisfy this album: 5 rejected for bitrate too low';
    const { container } = render(<ParkedExplanation state="PARKED" source="lidarr" detail={detail} />);

    expect(container.textContent).not.toContain(t.jobs.parkedLead);
    expect(container.textContent).toBe(`${t.jobs.parkedReason(detail)} ${t.jobs.parkedActions}`);
  });

  it('replaces the static lead with the job-specific reason for a manual job when a detail is supplied', () => {
    const detail = 'the download kept vanishing from the backend and ran out of retries, so automation stopped';
    const { container } = render(<ParkedExplanation state="PARKED" source="manual" detail={detail} />);

    expect(container.textContent).not.toContain(t.jobs.parkedLead);
    expect(container.textContent).toBe(`${t.jobs.parkedReason(detail)} ${t.jobs.parkedActionsManual}`);
  });

  it('renders nothing for a non-PARKED state, even with a detail present', () => {
    const { container } = render(
      <ParkedExplanation state="DOWNLOADING" source="lidarr" detail="the backend would not confirm cancelling this transfer, so automation stopped" />,
    );

    expect(container).toBeEmptyDOMElement();
  });

  // No ORPHANED case here on purpose. ORPHANED, the deprecated
  // pre-migration-0008 spelling of PARKED, never reaches this component as
  // itself — normalizeJobState (api/normalize.ts, covered by
  // normalize.test.ts) rewrites it to PARKED at the wire boundary, which is
  // why the prop type has no such member. Such a job therefore takes the
  // no-detail path above: it predates the job_parked event existing, so the
  // backend has no reason to send and the static copy renders, exactly as
  // before this change.
});
