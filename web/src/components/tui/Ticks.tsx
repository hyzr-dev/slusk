import { memo } from 'react';
import styles from './Ticks.module.css';

export type TickTone = 'bar' | 'ok' | 'bad' | 'queued';

const TONE_COLOR: Record<TickTone, string> = {
  bar: 'var(--bar)',
  ok: 'var(--ok)',
  bad: 'var(--bad)',
  queued: 'var(--tick-queued)',
};

interface Props {
  percent: number;
  tone?: TickTone;
  live?: boolean;
  height?: number;
}

/** Clamps to the drawable range — a bar never overshoots or goes negative. */
function clampPercent(percent: number): number {
  return Math.min(100, Math.max(0, percent));
}

function TicksImpl({ percent, tone = 'bar', live = false, height = 12 }: Props) {
  const clamped = clampPercent(percent);
  const color = TONE_COLOR[tone];
  // At 0% nothing is filled, so nothing may glow — an idle transfer must not
  // look like a moving one. Mirrors the old per-tick flare rule this
  // replaces (see git history of this file for the tick-based version).
  const flare = live && clamped > 0;

  return (
    <div className={styles.row} style={{ height }}>
      <div className={styles.track}>
        <span
          data-fill
          data-flare={flare ? 'true' : undefined}
          className={styles.fill}
          style={{
            width: `${clamped}%`,
            background: `repeating-linear-gradient(90deg,${color} 0 1px,transparent 1px 2px)`,
            // 40% is the closest color-mix equivalent of the mock's literal
            // `66` hex alpha suffix, which only works on literal hex colors —
            // our tones are CSS custom properties, so a suffix can't be
            // appended directly.
            boxShadow: flare ? `1px 0 4px color-mix(in srgb, ${color} 40%, transparent)` : undefined,
          }}
        />
      </div>
    </div>
  );
}

/**
 * A single-track progress bar that recolours and regrows in place as a
 * transfer advances (docs/design/slusk-tui.dc.html's `fill()` helper,
 * commit 688d52c — the dense-tick track from before that commit is now one
 * repeating-gradient track plus one gradient fill, not one <span> per tick).
 *
 * The old per-tick version was memoised on a fingerprint because a jobs list
 * polling every 3s could hold ~150 rows of 26 <span> ticks each; at two DOM
 * nodes per bar that concern is gone, so this compares the clamped percent
 * directly — which is also the *more* correct behaviour now, since a
 * continuous fill visibly moves on every percent, not just every tick
 * boundary.
 */
const Ticks = memo(TicksImpl, (prev, next) => {
  return (
    clampPercent(prev.percent) === clampPercent(next.percent) &&
    prev.tone === next.tone &&
    prev.live === next.live &&
    prev.height === next.height
  );
});

export default Ticks;
