import type { ThroughputSample } from '../../api/types';
import { formatShortTime, formatSpeed } from '../../format';
import { t } from '../../strings';
import styles from './ThroughputAreaChart.module.css';

const VIEW_WIDTH = 240;
const VIEW_HEIGHT = 44;

/**
 * The download-throughput area chart on the Overview page.
 *
 * Structurally a sibling of CumulativeAreaChart (same viewBox, same
 * empty-state and axis treatment) but not a variant of it: each sample here
 * is already an instantaneous rate (bytesPerSecond), so it is plotted
 * directly rather than accumulated into a running total the way hourly
 * counts are. The headline number is the peak rate in the window, and the
 * line connects samples directly (not a step chart) since a rate is
 * continuous between polls, unlike the fixed hourly buckets it's modelled on.
 */
export default function ThroughputAreaChart({ samples }: { samples: ThroughputSample[] }) {
  // Always [] (never absent) on a non-native backend or when unavailable —
  // see api/types.ts ChartsReport.throughput — so this is a real, expected
  // state, not just a pre-load gap.
  if (samples.length === 0) {
    return <div className={styles.empty}>{t.overview.noThroughputData}</div>;
  }

  const values = samples.map((s) => s.bytesPerSecond);
  const peak = Math.max(...values);
  // Clamped to 1 so an all-zero window divides cleanly instead of by zero -
  // every point then lands on the baseline.
  const max = peak > 0 ? peak : 1;

  const stepX = samples.length > 1 ? VIEW_WIDTH / (samples.length - 1) : 0;
  const points = values.map((v, i) => ({
    x: i * stepX,
    y: VIEW_HEIGHT - (v / max) * VIEW_HEIGHT,
  }));

  let line = `M ${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length; i++) {
    line += ` L ${points[i].x} ${points[i].y}`;
  }
  const lastX = points[points.length - 1].x;
  const area = `${line} L ${lastX} ${VIEW_HEIGHT} L 0 ${VIEW_HEIGHT} Z`;

  return (
    <div className={styles.wrap}>
      <div className={styles.peak}>{formatSpeed(peak)}</div>
      <svg
        className={styles.chart}
        viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
        role="img"
        aria-label={t.overview.throughputAriaLabel(formatSpeed(peak))}
      >
        <line x1={0} y1={VIEW_HEIGHT} x2={VIEW_WIDTH} y2={VIEW_HEIGHT} className={styles.baseline} />
        <path d={area} className={styles.area} />
        <path d={line} className={styles.line} />
      </svg>
      <div className={styles.axis}>
        <span>{formatShortTime(samples[0].at)}</span>
        <span>{t.overview.chartRangeEnd}</span>
      </div>
    </div>
  );
}
