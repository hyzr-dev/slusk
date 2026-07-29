import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import Ticks from './Ticks';

describe('Ticks', () => {
  it('renders a single fill sized to the percent', () => {
    const { container } = render(<Ticks percent={41} />);
    const fill = container.querySelector('[data-fill]') as HTMLElement;
    expect(fill).toBeInTheDocument();
    expect(fill.style.width).toBe('41%');
  });

  it('clamps out-of-range input rather than overshooting or going negative', () => {
    const over = render(<Ticks percent={140} />);
    expect((over.container.querySelector('[data-fill]') as HTMLElement).style.width).toBe('100%');

    const under = render(<Ticks percent={-20} />);
    expect((under.container.querySelector('[data-fill]') as HTMLElement).style.width).toBe('0%');
  });

  it('flares only when live and the bar is not empty', () => {
    const live = render(<Ticks percent={50} live />);
    expect(live.container.querySelectorAll('[data-flare="true"]')).toHaveLength(1);

    const still = render(<Ticks percent={50} />);
    expect(still.container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  it('does not flare at 0 percent, where nothing is filled', () => {
    const { container } = render(<Ticks percent={0} live />);
    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });
});
