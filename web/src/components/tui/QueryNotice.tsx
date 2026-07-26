import { t } from '../../strings';
import styles from './QueryNotice.module.css';

/**
 * The four states every GET in this app can be in from a view's point of
 * view. 'stale' is the one that is easy to miss: TanStack Query keeps `data`
 * across a failed background refetch (see App.tsx), which is what we want —
 * but until issue #201 nothing said so, and a failed *first* fetch fell
 * through the views' `?? []` defaults and rendered as "there is nothing here".
 */
export type QueryPhase = 'loading' | 'error' | 'ready' | 'stale';

/**
 * The subset of UseQueryResult this module reads. Declared structurally
 * rather than as Pick<UseQueryResult<T>, …> so one signature accepts queries
 * of any result type without a generic parameter at every call site.
 */
export interface ClassifiableQuery {
  data: unknown;
  isError: boolean;
  isPlaceholderData: boolean;
}

// Which phase wins when a region is fed by several queries. The only
// variadic call site (Health.tsx's metricsPhase) only ever uses the result to
// render a notice — hasData() is never called on a multi-query phase — so
// this is report-only: the worse *fact* about the group wins, and a failure
// the user should know about outranks a fetch that is merely still running.
const PRECEDENCE: Record<QueryPhase, number> = { ready: 0, loading: 1, stale: 2, error: 3 };

function ownPhase(query: ClassifiableQuery): QueryPhase {
  // `data !== undefined`, not truthiness: a legitimate 0, '' or [] response
  // is data. `isPlaceholderData` excludes the *previous* query key's response
  // — with `placeholderData: keepPreviousData` (App.tsx) navigating to a job
  // id never fetched before yields the previously viewed job's detail with
  // isLoading false, which is not this query's data and must not be rendered
  // as if it were.
  const own = query.data !== undefined && !query.isPlaceholderData;
  if (own) return query.isError ? 'stale' : 'ready';
  return query.isError ? 'error' : 'loading';
}

/**
 * Classify one query, or a region fed by several, into a single phase.
 *
 * Variadic because several views draw one region from more than one poll
 * (Overview's stat strip reads /status and /api/jobs; Health's metric table
 * reads /status, /api/uploads and /api/shares) and those regions want one
 * verdict, not one per source.
 */
export function queryPhase(...queries: readonly ClassifiableQuery[]): QueryPhase {
  let phase: QueryPhase = 'ready';
  for (const query of queries) {
    const own = ownPhase(query);
    if (PRECEDENCE[own] > PRECEDENCE[phase]) phase = own;
  }
  return phase;
}

/** Whether the region's real body can be rendered at all. */
export function hasData(phase: QueryPhase): boolean {
  return phase === 'ready' || phase === 'stale';
}

/**
 * The one-line report on a region's query state, rendered inside whichever
 * container holds that region's body — so a region with nothing to show has
 * this line in place of its body, and a region showing data that stopped
 * refreshing has it above that data.
 *
 * Renders nothing at all in the healthy case, which is what lets every view
 * use the same unconditional two-line pattern:
 *
 *   <QueryNotice phase={p} />
 *   {hasData(p) && ...the body, including its own EmptyState...}
 *
 * The node is always mounted, and role="status" with it. A live region
 * inserted at the same moment as its text is announced unreliably — the same
 * reason Shares.tsx keeps its rescan live region permanently mounted — so the
 * healthy case renders an empty container rather than null. `.silent` takes
 * that container out of flow entirely so it costs nothing in the flex and
 * grid layouts every call site sits in; see the comment there.
 */
export default function QueryNotice({ phase }: { phase: QueryPhase }) {
  const className =
    phase === 'ready' ? styles.silent : phase === 'loading' ? styles.loading : styles.failed;
  const message =
    phase === 'ready'
      ? null
      : phase === 'loading'
        ? t.query.loading
        : phase === 'error'
          ? t.query.failed
          : t.query.stale;

  return (
    <div role="status" className={className}>
      {message}
    </div>
  );
}
