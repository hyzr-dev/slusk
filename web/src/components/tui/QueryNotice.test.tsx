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

  it('renders nothing when ready', () => {
    const { container } = render(<QueryNotice phase="ready" />);
    expect(container.firstChild).toBeNull();
  });
});
