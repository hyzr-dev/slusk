import type { SearchPass } from '../../api/types';
import { formatShortTime } from '../../format';
import { t } from '../../strings';
import styles from './PassBarChart.module.css';

// Fixed slot count: the backend caps GET /api/charts at the 20 most recent
// passes, so the chart always reserves exactly 20 right-aligned slots -
// fewer than 20 passes just leaves empty slots on the left.
const SLOTS = 20;
const VIEW_WIDTH = 240;
const VIEW_HEIGHT = 48;
const GAP = 2;
const BAR_WIDTH = (VIEW_WIDTH - (SLOTS - 1) * GAP) / SLOTS;
const UNMATCHED_HEIGHT = 4;

export default function PassBarChart({ passes }: { passes: SearchPass[] }) {
  if (passes.length === 0) {
    return <div className={styles.empty}>{t.overview.noChartData}</div>;
  }

  const slotted = passes.slice(-SLOTS);
  const emptySlots = SLOTS - slotted.length;
  const matchedCount = slotted.filter((p) => p.matched > 0).length;

  return (
    <svg
      className={styles.chart}
      viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
      role="img"
      aria-label={t.overview.passesAriaLabel(matchedCount, slotted.length)}
    >
      {slotted.map((pass, i) => {
        const slot = emptySlots + i;
        const x = slot * (BAR_WIDTH + GAP);
        const matched = pass.matched > 0;
        const height = matched ? VIEW_HEIGHT : UNMATCHED_HEIGHT;
        const y = VIEW_HEIGHT - height;
        return (
          <rect
            key={slot}
            x={x}
            y={y}
            width={BAR_WIDTH}
            height={height}
            rx={2}
            className={matched ? styles.matched : styles.unmatched}
          >
            <title>{t.overview.passTooltip(formatShortTime(pass.startedAt), pass.matched)}</title>
          </rect>
        );
      })}
    </svg>
  );
}
