import { memo } from 'react';
import styles from './Ticks.module.css';

export type TickState = 'on' | 'partial' | 'off';
export type TickTone = 'bar' | 'ok' | 'bad' | 'queued';

const TONE_COLOR: Record<TickTone, string> = {
  bar: 'var(--bar)',
  ok: 'var(--ok)',
  bad: 'var(--bad)',
  queued: 'var(--tick-queued)',
};

/**
 * Which ticks are lit for a given fill percentage. Exported separately from
 * the component because the boundary behaviour — the single tick that
 * straddles the fill edge renders half-lit — is the only part worth testing,
 * and testing it through the DOM would mean asserting on inline styles.
 */
export function tickStates(percent: number, count: number): TickState[] {
  const clamped = Math.min(100, Math.max(0, percent));
  const filled = (clamped / 100) * count;
  const full = Math.floor(filled);
  return Array.from({ length: count }, (_, i) => {
    if (i < full) return 'on';
    if (i < filled) return 'partial';
    return 'off';
  });
}

/**
 * The render identity of a tick row.
 *
 * Two rows look the same only when they light the same number of full ticks
 * *and* agree on whether a partial tick trails them — a partial tick is drawn
 * at a fixed half opacity, so its exact fraction does not matter but its
 * presence does. floor and ceil together capture both facts.
 */
export function tickFingerprint(percent: number, count: number): string {
  const clamped = Math.min(100, Math.max(0, percent));
  const filled = (clamped / 100) * count;
  return `${Math.floor(filled)}:${Math.ceil(filled)}`;
}

interface Props {
  percent: number;
  count: number;
  tone?: TickTone;
  live?: boolean;
  height?: number;
}

function TicksImpl({ percent, count, tone = 'bar', live = false, height = 12 }: Props) {
  const states = tickStates(percent, count);
  // The head is the last fully lit tick. At 0% there is none, so nothing
  // flares — an idle transfer must not look like a moving one.
  const head = states.lastIndexOf('on');
  const color = TONE_COLOR[tone];

  return (
    <div className={styles.row} style={{ height }}>
      {states.map((state, i) => {
        const flare = live && i === head && head >= 0;
        return (
          <span
            key={i}
            data-tick
            data-flare={flare ? 'true' : undefined}
            className={flare ? `${styles.tick} ${styles.flare}` : styles.tick}
            style={{
              background: state === 'off' ? 'var(--tick-off)' : color,
              opacity: state === 'partial' ? 0.5 : 1,
            }}
          />
        );
      })}
    </div>
  );
}

/**
 * A dense bar of uniform ticks that recolour in place as a transfer advances.
 *
 * Memoised on the fingerprint of lit and partial ticks rather than on raw `percent`: the
 * jobs list polls every 3 s and can hold ~150 rows of 26 ticks each, so a job
 * creeping from 41.2 % to 41.4 % must not repaint ~3900 nodes for a bar that
 * looks identical.
 */
const Ticks = memo(TicksImpl, (prev, next) => {
  return (
    tickFingerprint(prev.percent, prev.count) === tickFingerprint(next.percent, next.count) &&
    prev.count === next.count &&
    prev.tone === next.tone &&
    prev.live === next.live &&
    prev.height === next.height
  );
});

export default Ticks;
