import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { SearchPass } from '../../api/types';
import PassBarChart from './PassBarChart';

function makePass(startedAt: string, matched: number): SearchPass {
  return { startedAt, finishedAt: startedAt, searched: 1, matched };
}

describe('PassBarChart', () => {
  it('renders one bar per pass', () => {
    const passes = [makePass('2026-07-01T10:00:00Z', 1), makePass('2026-07-01T10:05:00Z', 0)];
    const { container } = render(<PassBarChart passes={passes} />);
    expect(container.querySelectorAll('rect')).toHaveLength(2);
  });

  it('distinguishes matched bars from unmatched bars', () => {
    const passes = [makePass('2026-07-01T10:00:00Z', 1), makePass('2026-07-01T10:05:00Z', 0)];
    const { container } = render(<PassBarChart passes={passes} />);
    const [matchedRect, unmatchedRect] = container.querySelectorAll('rect');
    expect(matchedRect.getAttribute('height')).not.toBe(unmatchedRect.getAttribute('height'));
  });

  it('shows the empty state when there are no passes', () => {
    render(<PassBarChart passes={[]} />);
    expect(screen.getByText('No pass history yet')).toBeInTheDocument();
  });

  it('sets an aria-label summarizing matched vs total passes', () => {
    const passes = [makePass('2026-07-01T10:00:00Z', 1), makePass('2026-07-01T10:05:00Z', 0)];
    render(<PassBarChart passes={passes} />);
    expect(
      screen.getByRole('img', { name: '1 of 2 recent search passes matched' }),
    ).toBeInTheDocument();
  });
});
