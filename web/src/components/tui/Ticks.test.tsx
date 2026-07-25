import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import Ticks, { tickStates } from './Ticks';

describe('tickStates', () => {
  it('marks every tick off at 0 percent', () => {
    expect(tickStates(0, 4)).toEqual(['off', 'off', 'off', 'off']);
  });

  it('marks every tick on at 100 percent', () => {
    expect(tickStates(100, 4)).toEqual(['on', 'on', 'on', 'on']);
  });

  it('marks the tick straddling the boundary as partial', () => {
    // 3 of 8 ticks filled exactly, so no tick straddles the edge.
    expect(tickStates(37.5, 8)).toEqual([
      'on', 'on', 'on', 'off', 'off', 'off', 'off', 'off',
    ]);
    // 3.2 ticks filled: the fourth is partially covered.
    expect(tickStates(40, 8)).toEqual([
      'on', 'on', 'on', 'partial', 'off', 'off', 'off', 'off',
    ]);
  });

  it('clamps out-of-range input rather than drawing extra or negative ticks', () => {
    expect(tickStates(140, 3)).toEqual(['on', 'on', 'on']);
    expect(tickStates(-20, 3)).toEqual(['off', 'off', 'off']);
  });
});

describe('Ticks', () => {
  it('renders exactly count elements', () => {
    const { container } = render(<Ticks percent={50} count={26} />);
    expect(container.querySelectorAll('[data-tick]')).toHaveLength(26);
  });

  it('flares only the newest filled tick, and only when live', () => {
    const live = render(<Ticks percent={50} count={4} live />);
    expect(live.container.querySelectorAll('[data-flare="true"]')).toHaveLength(1);

    const still = render(<Ticks percent={50} count={4} />);
    expect(still.container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });

  it('does not flare at 0 percent, where there is no filled tick', () => {
    const { container } = render(<Ticks percent={0} count={4} live />);
    expect(container.querySelectorAll('[data-flare="true"]')).toHaveLength(0);
  });
});
