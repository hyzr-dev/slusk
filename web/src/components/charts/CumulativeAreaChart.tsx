import type { HourCount } from '../../api/types';
import { t } from '../../strings';
import styles from './CumulativeAreaChart.module.css';

const VIEW_WIDTH = 240;
const VIEW_HEIGHT = 44;

export default function CumulativeAreaChart({ buckets }: { buckets: HourCount[] }) {
  if (buckets.length === 0) return null;

  let running = 0;
  const cumulative = buckets.map((b) => (running += b.count));
  const total = cumulative[cumulative.length - 1];
  // Clamped to 1 so an all-zero series divides cleanly instead of by zero -
  // every point then lands on the baseline, which is exactly the flat line
  // the all-zero case should render.
  const max = total > 0 ? total : 1;

  const stepX = buckets.length > 1 ? VIEW_WIDTH / (buckets.length - 1) : 0;
  const points = cumulative.map((c, i) => ({
    x: i * stepX,
    y: VIEW_HEIGHT - (c / max) * VIEW_HEIGHT,
  }));

  let line = `M ${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length; i++) {
    line += ` H ${points[i].x} V ${points[i].y}`;
  }
  const lastX = points[points.length - 1].x;
  const area = `${line} L ${lastX} ${VIEW_HEIGHT} L 0 ${VIEW_HEIGHT} Z`;

  return (
    <div className={styles.wrap}>
      <div className={styles.total}>{total}</div>
      <svg
        className={styles.chart}
        viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
        role="img"
        aria-label={t.overview.completedAriaLabel(total)}
      >
        <line x1={0} y1={VIEW_HEIGHT} x2={VIEW_WIDTH} y2={VIEW_HEIGHT} className={styles.baseline} />
        <path d={area} className={styles.area} />
        <path d={line} className={styles.line} />
      </svg>
      <div className={styles.axis}>
        <span>{t.overview.chartRangeStart}</span>
        <span>{t.overview.chartRangeEnd}</span>
      </div>
    </div>
  );
}
