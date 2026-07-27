import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import styles from './SectionHeader.module.css';
import SectionHeader from './SectionHeader';

describe('SectionHeader', () => {
  // Every panel in the app opens with this component; if its label ever
  // stopped being a heading, the whole document would lose its heading
  // outline (the regression this test guards against — see #198 task 14's
  // fix report).
  it('renders its label as a heading without truncation or redundant title metadata by default', () => {
    render(<SectionHeader label="THROUGHPUT" meta="last 24h" />);

    const heading = screen.getByRole('heading', { name: 'THROUGHPUT', level: 2 });
    expect(heading).not.toHaveClass(styles.truncateLabel);
    expect(heading).not.toHaveAttribute('title');
    expect(screen.getByText('last 24h')).toBeInTheDocument();
  });

  it('opts a label into visual truncation without changing its heading text', () => {
    const label = 'a-username-that-is-much-wider-than-the-header';
    render(<SectionHeader label={label} meta="ONLINE" truncateLabel />);

    const heading = screen.getByRole('heading', { level: 2, name: label });
    expect(heading).toHaveClass(styles.truncateLabel);
    expect(heading).toHaveTextContent(label);
    expect(heading).not.toHaveAttribute('title');
    expect(screen.getByText('ONLINE')).toBeInTheDocument();
  });
});
