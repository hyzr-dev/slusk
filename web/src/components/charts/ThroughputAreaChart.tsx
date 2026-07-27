import type { ThroughputSample } from '../../api/types';
import { formatShortTime, formatSpeed } from '../../format';
import { t } from '../../strings';
import styles from './ThroughputAreaChart.module.css';

const VIEW_WIDTH = 240;
const VIEW_HEIGHT = 44;

export type ThroughputDirection = 'download' | 'upload';

/**
 * One direction of the recent-throughput window on the Overview page.
 *
 * Each instance calculates its own peak and scale. Keeping the directions in
 * separate SVGs prevents a fast download from flattening the upload series (or
 * vice versa), while the colour and text labels make the two plots readable
 * without relying on colour alone.
 */
export default function ThroughputAreaChart({
  samples,
  direction,
}: {
  samples: ThroughputSample[];
  direction: ThroughputDirection;
}) {
  const upload = direction === 'upload';
  const directionLabel = upload ? t.overview.uploadThroughput : t.overview.downloadThroughput;
  const emptyLabel = upload
    ? t.overview.noUploadThroughputData
    : t.overview.noDownloadThroughputData;

  const values = samples.map((s) => s.bytesPerSecond);
  const peak = values.length > 0 ? Math.max(...values) : 0;

  return (
    <div className={`${styles.wrap} ${upload ? styles.upload : styles.download}`}>
      <div className={styles.head}>
        <span className={styles.direction}>{directionLabel}</span>
        {samples.length > 0 && (
          <span className={styles.peak}>
            <span className={styles.peakLabel}>{t.overview.peak}</span>{' '}
            {formatSpeed(peak)}
          </span>
        )}
      </div>

      {samples.length === 0 ? (
        <div className={styles.empty}>{emptyLabel}</div>
      ) : (
        <Chart samples={samples} values={values} peak={peak} direction={direction} />
      )}
    </div>
  );
}

function Chart({
  samples,
  values,
  peak,
  direction,
}: {
  samples: ThroughputSample[];
  values: number[];
  peak: number;
  direction: ThroughputDirection;
}) {
  // Clamped to 1 so an all-zero window divides cleanly instead of by zero —
  // every point then lands on the baseline.
  const max = peak > 0 ? peak : 1;
  const stepX = samples.length > 1 ? VIEW_WIDTH / (samples.length - 1) : 0;
  const points = values.map((value, index) => ({
    x: index * stepX,
    y: VIEW_HEIGHT - (value / max) * VIEW_HEIGHT,
  }));

  let line = `M ${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length; i++) {
    line += ` L ${points[i].x} ${points[i].y}`;
  }
  const lastX = points[points.length - 1].x;
  const area = `${line} L ${lastX} ${VIEW_HEIGHT} L 0 ${VIEW_HEIGHT} Z`;
  const ariaLabel = direction === 'upload'
    ? t.overview.uploadThroughputAriaLabel(formatSpeed(peak))
    : t.overview.downloadThroughputAriaLabel(formatSpeed(peak));

  return (
    <>
      <svg
        className={styles.chart}
        viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
        role="img"
        aria-label={ariaLabel}
      >
        <line x1={0} y1={VIEW_HEIGHT} x2={VIEW_WIDTH} y2={VIEW_HEIGHT} className={styles.baseline} />
        <path d={area} className={styles.area} />
        <path d={line} className={styles.line} />
      </svg>
      <div className={styles.axis}>
        <span>{formatShortTime(samples[0].at)}</span>
        <span>{t.overview.chartRangeEnd}</span>
      </div>
    </>
  );
}
