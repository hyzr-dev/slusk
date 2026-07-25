import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import Ticks, { tickStates, tickFingerprint } from './Ticks';

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

describe('tickFingerprint', () => {
  it('separates a bar with a partial tick from one without', () => {
    // filled = 3.0 exactly: three lit ticks, nothing trailing.
    // filled = 3.04: the same three, plus a half-lit fourth.
    expect(tickFingerprint(37.5, 8)).not.toBe(tickFingerprint(38, 8));
  });

  it('treats any two partial fractions in the same tick as identical', () => {
    // A partial tick renders at a fixed opacity, so 3.04 and 3.9 look alike.
    expect(tickFingerprint(38, 8)).toBe(tickFingerprint(48, 8));
  });

  it('separates a full tick from a partial one at the same ceiling', () => {
    expect(tickFingerprint(50, 8)).not.toBe(tickFingerprint(38, 8));
  });

  it('does not freeze a bar leaving zero', () => {
    // The failure this replaced: floor stays 0 below one whole tick, so a
    // transfer that had just started never repainted.
    expect(tickFingerprint(0, 26)).not.toBe(tickFingerprint(1, 26));
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
