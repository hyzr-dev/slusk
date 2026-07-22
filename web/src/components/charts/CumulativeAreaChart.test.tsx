import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { HourCount } from '../../api/types';
import CumulativeAreaChart from './CumulativeAreaChart';

function makeBuckets(counts: number[]): HourCount[] {
  return counts.map((count, i) => ({
    hour: `2026-07-01T${String(i).padStart(2, '0')}:00:00Z`,
    count,
  }));
}

describe('CumulativeAreaChart', () => {
  it('shows the cumulative total for known buckets', () => {
    render(<CumulativeAreaChart buckets={makeBuckets([1, 2, 0, 3])} />);
    expect(screen.getByText('6')).toBeInTheDocument();
  });

  it('renders an all-zero series as a flat baseline with a total of 0', () => {
    render(<CumulativeAreaChart buckets={makeBuckets([0, 0, 0, 0])} />);
    expect(screen.getByText('0')).toBeInTheDocument();
  });

  it('shows the empty state when there are no buckets', () => {
    render(<CumulativeAreaChart buckets={[]} />);
    expect(screen.getByText('No pass history yet')).toBeInTheDocument();
  });
});
