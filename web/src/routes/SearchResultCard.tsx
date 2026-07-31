import type { ReactNode } from 'react';
import type { SearchGroup } from '../api/types';
import { formatBitrateLabel, formatQualityLabel } from '../api/normalize';
import Button from '../components/tui/Button';
import { formatDuration, formatSize, formatSpeed, formatTrackDuration } from '../format';
import { t } from '../strings';
import styles from './SearchResultCard.module.css';

// Deliberately NOT role="table"/"grid": a group card holds a real <button>
// header and, once expanded, real checkboxes, and an ARIA cell/columnheader
// wrapper around either one demotes it to inline and silently halves its tap
// target (see the aria-wrapper-shrinks-grid-click-targets lesson this file
// exists to avoid repeating). <ul>/<li> of <article> instead, mirroring
// Jobs.tsx's row/expansion *machinery* — memoized row component, a
// button[aria-expanded] header, a separate disclosure body — without
// reusing its role="table" grid.

export interface CardStatus {
  queued: boolean;
  error?: string;
}

interface Props {
  group: SearchGroup;
  best: boolean;
  expanded: boolean;
  onToggleExpand: () => void;
  selectedFiles: Set<string>;
  onToggleFile: (filename: string) => void;
  status?: CardStatus;
  onDownloadAlbum: () => void;
  onDownloadSelected: () => void;
  // The #59 seam (issue #58 §13): Search.tsx does not pass this. A future
  // "Koppla till Lidarr" panel renders here, between the track expansion and
  // the actions row, with no markup or state added on this side until then.
  renderImportPanel?: (group: SearchGroup) => ReactNode;
}

// Badges derived from whatever attributes the peer actually reported (issue
// #58 §1/§4) — format, sample-rate/bit-depth quality, and bitrate/VBR.
// Exported so Search.test.tsx can assert on it directly without depending on
// DOM order.
export function cardBadges(group: SearchGroup): string[] {
  const badges: string[] = [];
  if (group.format) badges.push(group.format.toUpperCase());
  const quality = formatQualityLabel(group);
  if (quality) badges.push(quality);
  const bitrate = formatBitrateLabel(group);
  if (bitrate) badges.push(bitrate);
  return badges;
}

// The one-line stat strip under the badges. `trackCount` counts audio files
// only while both download buttons enqueue the whole folder, so the extras are
// named rather than left to contradict "Download selected (13)". The album
// duration is rendered only when the server set it, which it does only when
// EVERY file in the group reported one (searchGroupDTO's honesty gate) — an
// unset value means unknown, never zero.
function cardMeta(group: SearchGroup): string {
  const parts = [t.search.trackCount(group.trackCount)];
  const extras = group.files.length - group.trackCount;
  if (extras > 0) parts.push(t.search.extraFiles(extras));
  if (group.durationSeconds !== undefined) parts.push(formatDuration(group.durationSeconds));
  parts.push(formatSize(group.sizeBytes));
  return parts.join(' · ');
}

export default function SearchResultCard({
  group,
  best,
  expanded,
  onToggleExpand,
  selectedFiles,
  onToggleFile,
  status,
  onDownloadAlbum,
  onDownloadSelected,
  renderImportPanel,
}: Props) {
  const expansionId = `search-expansion-${group.id}`;
  const queued = status?.queued ?? false;

  return (
    <li className={styles.item}>
      <article className={`${styles.card} ${best ? styles.best : ''}`}>
        {best && <span className={styles.bestBadge}>{t.search.bestMatch}</span>}

        <button
          type="button"
          className={styles.header}
          aria-expanded={expanded}
          // Only while the container it names actually exists: the expansion
          // is unmounted when collapsed, and aria-controls pointing at a
          // missing id is worse than no aria-controls at all.
          aria-controls={expanded ? expansionId : undefined}
          onClick={onToggleExpand}
        >
          <span className={styles.titleBlock}>
            <span className={styles.titleRow}>
              <span className={styles.title}>{group.title}</span>
              {/* NOT the artist. This is the peer's parent DIRECTORY name;
                  the artist is not on the Soulseek wire at all (issue #58
                  §4). Rendered in the mono face with a trailing slash so it
                  reads as a path segment rather than as the "Album — Artist"
                  idiom the design's dim-text-after-title slot otherwise
                  implies, with a hidden label and a title for anyone who
                  can't see that treatment. See t.search.folderLabel. */}
              <span className={styles.parent} title={t.search.folderTitle(group.parent)}>
                <span className={styles.srOnly}>{t.search.folderLabel} </span>
                {group.parent}/
              </span>
            </span>
            <span className={styles.badgeRow}>
              {cardBadges(group).map((badge) => (
                <span key={badge} className={styles.badge}>{badge}</span>
              ))}
              <span className={styles.meta}>{cardMeta(group)}</span>
            </span>
          </span>
          <span className={styles.availBlock}>
            <span className={group.freeUploadSlot ? styles.availOk : styles.availDim}>
              {group.freeUploadSlot
                ? t.search.freeSlot
                : group.queueLength > 0
                  ? t.search.queuePosition(group.queueLength)
                  : t.search.noFreeSlot}
            </span>
            <span className={styles.peerSpeed}>{t.search.peerAndSpeed(group.peer, formatSpeed(group.uploadSpeed))}</span>
          </span>
          <span aria-hidden className={styles.chevron}>{expanded ? '▾' : '▸'}</span>
        </button>

        {expanded && (
          <div id={expansionId} className={styles.expansion}>
            <fieldset className={styles.fileList}>
              <legend className={styles.srOnly}>{t.jobs.files}</legend>
              {group.files.map((file, i) => (
                <label key={file.filename} className={styles.fileRow}>
                  <input
                    type="checkbox"
                    checked={selectedFiles.has(file.filename)}
                    onChange={() => onToggleFile(file.filename)}
                  />
                  <span className={styles.fileNum}>{i + 1}</span>
                  <span className={styles.fileName} title={file.name}>{file.name}</span>
                  <span className={styles.fileDur}>{formatTrackDuration(file.durationSeconds)}</span>
                  <span className={styles.fileSize}>{formatSize(file.size)}</span>
                </label>
              ))}
            </fieldset>
          </div>
        )}

        {renderImportPanel?.(group)}

        <div className={styles.actions}>
          <Button variant="primary" onClick={onDownloadAlbum} disabled={queued}>
            {t.search.downloadAlbum}
          </Button>
          <Button variant="ghost" onClick={onDownloadSelected} disabled={queued || selectedFiles.size === 0}>
            {t.search.downloadSelected(selectedFiles.size)}
          </Button>
          <span className={styles.folderPath} title={group.folder}>{group.folder}</span>
        </div>

        {queued && <div className={styles.notice} role="status">{t.search.queuedNotice}</div>}
        {status?.error && <div className={styles.error} role="alert">{status.error}</div>}
      </article>
    </li>
  );
}
