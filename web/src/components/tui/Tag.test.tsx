import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Tag, { tagFor } from './Tag';
import { t } from '../../strings';

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

  it('reports a manual job downloaded without an albumMbid as not imported (issue #59)', () => {
    expect(tagFor('notImported')).toBe('NI');
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

  it('renders NI with the title from strings (issue #59)', () => {
    render(<Tag status="notImported" />);
    const el = screen.getByText('NI');
    // Asserted against t, not a second copy of the sentence: the wording is
    // owned by strings.ts, and duplicating it here means every rewording has
    // to be made twice, failing for a reason that is not a defect.
    expect(el).toHaveAttribute('title', t.tagTitle.NI);
    // What does need pinning is that it never reads as a failure — nothing
    // went wrong, the download succeeded and the files are on disk.
    expect(t.tagTitle.NI).not.toMatch(/fail|error/i);
  });
});
