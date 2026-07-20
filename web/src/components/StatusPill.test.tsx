import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import StatusPill from './StatusPill';

describe('StatusPill', () => {
  it('shows the translated state label', () => {
    render(<StatusPill status="active" state="DOWNLOADING" />);
    expect(screen.getByText('Downloading')).toBeInTheDocument();
  });

  // Legacy behaviour: an unrecognised state falls back to the coarser status
  // field, not to the raw state string. See dashboard.js:213.
  it('falls back to the status label for an unknown state', () => {
    render(<StatusPill status="queued" state={'FUTURE_STATE' as never} />);
    expect(screen.getByText('Queued')).toBeInTheDocument();
  });
});
