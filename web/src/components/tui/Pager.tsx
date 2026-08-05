import { t } from '../../strings';
import styles from './Pager.module.css';

type PageItem = number | 'ellipsis';

// Always exposes the boundaries while keeping the control compact for large
// collections. A one-page neighbourhood around the current page is enough to
// move locally; first/last provide the long jump. Extracted from Jobs.tsx
// (#425) so the Peers view can mount the same control without growing a
// second, subtly different pager next to this one.
export function paginationItems(page: number, totalPages: number): PageItem[] {
  if (totalPages <= 7) return Array.from({ length: totalPages }, (_, i) => i);
  const pages = [...new Set([0, totalPages - 1, page - 1, page, page + 1])]
    .filter((candidate) => candidate >= 0 && candidate < totalPages)
    .sort((a, b) => a - b);
  const items: PageItem[] = [];
  pages.forEach((candidate, index) => {
    if (index > 0 && candidate - pages[index - 1] > 1) items.push('ellipsis');
    items.push(candidate);
  });
  return items;
}

export interface PagerProps {
  page: number;
  totalPages: number;
  onChange: (page: number) => void;
}

// The prev/numbered/next control itself. No Jobs-specific props: a caller
// that wants an accessible-name for the surrounding <nav>, or a result-count
// summary beside it (Jobs does both — see .pagination/.resultRange in
// Jobs.module.css), owns and renders those itself around this component.
export default function Pager({ page, totalPages, onChange }: PagerProps) {
  return (
    <div className={styles.pageButtons}>
      <button type="button" className={styles.pageButton} disabled={page === 0} onClick={() => onChange(page - 1)}>
        {t.pager.previousPage}
      </button>
      {paginationItems(page, totalPages).map((item, index) =>
        item === 'ellipsis' ? (
          <span key={`ellipsis-${index}`} className={styles.ellipsis} aria-hidden>…</span>
        ) : (
          <button
            type="button"
            key={item}
            className={`${styles.pageButton} ${item === page ? styles.pageButtonActive : ''}`}
            aria-current={item === page ? 'page' : undefined}
            aria-label={t.pager.pageLabel(item + 1)}
            onClick={() => onChange(item)}
          >
            {item + 1}
          </button>
        ),
      )}
      <button type="button" className={styles.pageButton} disabled={page + 1 >= totalPages} onClick={() => onChange(page + 1)}>
        {t.pager.nextPage}
      </button>
    </div>
  );
}
