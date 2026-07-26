import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { t } from '../../strings';
import QueryNotice, { hasData, queryPhase, type ClassifiableQuery } from './QueryNotice';

function query(overrides: Partial<ClassifiableQuery>): ClassifiableQuery {
  return { data: undefined, isError: false, isPlaceholderData: false, ...overrides };
}

describe('queryPhase', () => {
  it('is loading before any data or error arrives', () => {
    expect(queryPhase(query({ data: undefined, isError: false }))).toBe('loading');
  });

  it('is error when it never had data and the fetch failed', () => {
    expect(queryPhase(query({ data: undefined, isError: true }))).toBe('error');
  });

  it('is ready once data has arrived without error', () => {
    expect(queryPhase(query({ data: [], isError: false }))).toBe('ready');
  });

  it('treats a falsy-but-defined value as data, not as absent', () => {
    expect(queryPhase(query({ data: 0, isError: false }))).toBe('ready');
  });

  it('is stale when data is present but the last fetch failed', () => {
    expect(queryPhase(query({ data: [], isError: true }))).toBe('stale');
  });

  it('does not count placeholder data as this query’s own data', () => {
    expect(queryPhase(query({ data: [{}], isError: false, isPlaceholderData: true }))).toBe('loading');
    expect(queryPhase(query({ data: [{}], isError: true, isPlaceholderData: true }))).toBe('error');
  });

  it('picks the worst phase across several queries feeding one region', () => {
    const ready = query({ data: [], isError: false });
    const loading = query({ data: undefined, isError: false });
    const error = query({ data: undefined, isError: true });
    const stale = query({ data: [], isError: true });

    expect(queryPhase(ready, loading)).toBe('loading');
    expect(queryPhase(loading, error)).toBe('error');
    expect(queryPhase(ready, stale)).toBe('stale');
    expect(queryPhase(ready, ready)).toBe('ready');
    // stale outranks loading: a query that has data but failed to refresh is
    // a real failure the reader should be told about, and must not be masked
    // by a sibling query that happens to still be in flight.
    expect(queryPhase(stale, loading)).toBe('stale');
  });
});

describe('hasData', () => {
  it('is true only for ready and stale', () => {
    expect(hasData('ready')).toBe(true);
    expect(hasData('stale')).toBe(true);
    expect(hasData('loading')).toBe(false);
    expect(hasData('error')).toBe(false);
  });
});

describe('QueryNotice', () => {
  it('renders the loading line', () => {
    render(<QueryNotice phase="loading" />);
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
  });

  it('renders the failed line', () => {
    render(<QueryNotice phase="error" />);
    expect(screen.getByText(t.query.failed)).toBeInTheDocument();
  });

  it('renders the stale line', () => {
    render(<QueryNotice phase="stale" />);
    expect(screen.getByText(t.query.stale)).toBeInTheDocument();
  });

  it('renders no text when ready', () => {
    render(<QueryNotice phase="ready" />);
    expect(screen.queryByText(t.query.loading)).not.toBeInTheDocument();
    expect(screen.queryByText(t.query.failed)).not.toBeInTheDocument();
    expect(screen.queryByText(t.query.stale)).not.toBeInTheDocument();
  });

  // The live region has to exist before it has anything to say. A role="status"
  // node inserted at the same moment as its text is announced unreliably, so
  // the healthy case keeps an empty container mounted rather than returning
  // null (#208). Whether that container is visually out of flow is a CSS
  // question jsdom cannot answer — see the PR for the measured check.
  it.each(['ready', 'loading', 'error', 'stale'] as const)(
    'keeps the live region mounted in the %s phase',
    (phase) => {
      render(<QueryNotice phase={phase} />);
      expect(screen.getByRole('status')).toBeInTheDocument();
    },
  );

  it('announces the failure through the live region rather than beside it', () => {
    render(<QueryNotice phase="error" />);
    expect(screen.getByRole('status')).toHaveTextContent(t.query.failed);
  });
});
