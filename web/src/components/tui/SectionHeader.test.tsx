import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import SectionHeader from './SectionHeader';

describe('SectionHeader', () => {
  // Every panel in the app opens with this component; if its label ever
  // stopped being a heading, the whole document would lose its heading
  // outline (the regression this test guards against — see #198 task 14's
  // fix report).
  it('renders its label as a heading', () => {
    render(<SectionHeader label="THROUGHPUT" />);
    expect(screen.getByRole('heading', { name: 'THROUGHPUT', level: 2 })).toBeInTheDocument();
  });

  it('keeps a long label available while rendering meta beside it', () => {
    const label = 'a-username-that-is-much-wider-than-the-header';
    render(<SectionHeader label={label} meta="ONLINE" />);

    expect(screen.getByRole('heading', { level: 2, name: label })).toHaveAttribute('title', label);
    expect(screen.getByText('ONLINE')).toBeInTheDocument();
  });
});
