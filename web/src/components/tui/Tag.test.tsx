import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Tag, { tagFor } from './Tag';

describe('tagFor', () => {
  it('maps each job status to its tag', () => {
    expect(tagFor('queued', 'WANTED')).toBe('QU');
    expect(tagFor('active', 'DOWNLOADING')).toBe('DL');
    expect(tagFor('stalled', 'DOWNLOADING')).toBe('ST');
    expect(tagFor('done', 'DONE')).toBe('OK');
    expect(tagFor('failed', 'FAILED')).toBe('FA');
    expect(tagFor('orphaned', 'ORPHANED')).toBe('OR');
  });

  it('reports importing from the state, which the status cannot express', () => {
    expect(tagFor('active', 'IMPORTING')).toBe('IM');
  });

  it('reports a job waiting in a peer queue as queued, not downloading', () => {
    // This is the case StatusPill could not express (issue #190): the job is
    // active, but no bytes move until the peer reaches us in its own queue.
    expect(tagFor('active', 'DOWNLOADING', 4)).toBe('QU');
    expect(tagFor('active', 'DOWNLOADING', 0)).toBe('DL');
    expect(tagFor('active', 'DOWNLOADING', undefined)).toBe('DL');
  });

  it('does not let a peer queue position override a terminal status', () => {
    expect(tagFor('done', 'DONE', 3)).toBe('OK');
    expect(tagFor('failed', 'FAILED', 3)).toBe('FA');
  });
});

describe('Tag', () => {
  it('renders the tag text with a readable title', () => {
    render(<Tag status="stalled" state="DOWNLOADING" />);
    const el = screen.getByText('ST');
    expect(el).toHaveAttribute('title', 'Stalled');
  });
});
