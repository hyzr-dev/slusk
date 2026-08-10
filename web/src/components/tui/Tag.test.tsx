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

  it('reports a job Lidarr permanently refused as import refused (issue #470)', () => {
    expect(tagFor('importRefused')).toBe('IR');
  });

  // Issue #416 split wanted/selecting/waiting out of queued/active, and
  // moved the "waiting in a peer queue" fact (formerly a client-side
  // queuePosition check on an 'active' job — issue #190) onto the backend's
  // own 'queued' status. tagFor no longer takes a queuePosition at all.
  it('maps the three new pre-transfer statuses to their own tags', () => {
    expect(tagFor('wanted')).toBe('WA');
    expect(tagFor('selecting')).toBe('SE');
    expect(tagFor('queued')).toBe('QU');
  });
});

describe('Tag', () => {
  it('renders the tag text with a readable title', () => {
    render(<Tag status="parked" />);
    const el = screen.getByText('PA');
    expect(el).toHaveAttribute('title', 'Parked');
  });

  it('renders the three new pre-transfer statuses with their own tags and titles (issue #416)', () => {
    render(<Tag status="wanted" />);
    expect(screen.getByText('WA')).toHaveAttribute('title', t.tagTitle.WA);
    render(<Tag status="selecting" />);
    expect(screen.getByText('SE')).toHaveAttribute('title', t.tagTitle.SE);
    render(<Tag status="waiting" />);
    expect(screen.getByText('WT')).toHaveAttribute('title', t.tagTitle.WT);
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

  it('renders IR with the title from strings and a --bad tone (issue #470)', () => {
    render(<Tag status="importRefused" />);
    const el = screen.getByText('IR');
    expect(el).toHaveAttribute('title', t.tagTitle.IR);
    // Unlike NI, IR IS a terminal outcome that needs a person's attention —
    // it must carry --bad, the same class as FA/ST/PA. Asserted via the CSS
    // module class name it renders with, mirroring the class-name check
    // elsewhere in this suite.
    expect(el.className).toMatch(/bad/);
  });
});
