import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Tag, { tagFor } from './Tag';

describe('tagFor', () => {
  it('maps each job status to its tag', () => {
    expect(tagFor('queued')).toBe('QU');
    expect(tagFor('active')).toBe('DL');
    expect(tagFor('stalled')).toBe('ST');
    expect(tagFor('done')).toBe('OK');
    expect(tagFor('failed')).toBe('FA');
    expect(tagFor('parked')).toBe('PA');
  });

  it('reports importing from the status (issue #269 — the backend used to serialize it as active)', () => {
    expect(tagFor('importing')).toBe('IM');
  });

  it('reports a job waiting in a peer queue as queued, not downloading', () => {
    // This is the case StatusPill could not express (issue #190): the job is
    // active, but no bytes move until the peer reaches us in its own queue.
    expect(tagFor('active', 4)).toBe('QU');
    expect(tagFor('active', 0)).toBe('DL');
    expect(tagFor('active', undefined)).toBe('DL');
  });

  it('does not let a peer queue position override a terminal status', () => {
    expect(tagFor('done', 3)).toBe('OK');
    expect(tagFor('failed', 3)).toBe('FA');
  });
});

describe('Tag', () => {
  it('renders the tag text with a readable title', () => {
    render(<Tag status="parked" />);
    const el = screen.getByText('PA');
    expect(el).toHaveAttribute('title', 'Parked');
  });
});
