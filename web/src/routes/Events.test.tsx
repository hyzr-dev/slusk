import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { JobEvent } from '../api/types';
import { t } from '../strings';
import Events from './Events';

afterEach(() => vi.unstubAllGlobals());

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function stubFetchFailing() {
  vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('boom'))));
}

function renderEvents(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Events />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Events query state', () => {
  it('shows the loading line, not the empty message, before the first fetch resolves', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderEvents(client);
    expect(screen.getByText(t.query.loading)).toBeInTheDocument();
    expect(screen.queryByText(t.events.empty, { exact: false })).not.toBeInTheDocument();
    expect(screen.getByText(t.columns.event)).toBeInTheDocument();
  });

  it('shows the failed line when the fetch never succeeds', async () => {
    stubFetchFailing();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderEvents(client);
    expect(await screen.findByText(t.query.failed)).toBeInTheDocument();
    expect(screen.queryByText(t.events.empty, { exact: false })).not.toBeInTheDocument();
  });

  it('shows the empty message, and no notice, once the fetch resolves with no events', () => {
    stubFetchIndefinitely();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(queryKeys.events, []);
    renderEvents(client);
    expect(screen.getByText(t.events.empty, { exact: false })).toBeInTheDocument();
    expect(screen.queryByText(t.query.loading)).not.toBeInTheDocument();
    expect(screen.queryByText(t.query.failed)).not.toBeInTheDocument();
  });
});

function makeEvent(overrides: Partial<JobEvent> = {}): JobEvent {
  return {
    id: 1,
    jobId: 42,
    event: 'search_fallback',
    detail: 'fell back to a looser query',
    createdAt: '2026-07-20T14:32:05Z',
    ...overrides,
  };
}

function renderEventsWith(events: JobEvent[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(queryKeys.events, events);
  stubFetchIndefinitely();
  return renderEvents(client);
}

describe('Events rows', () => {
  it('renders four cells per event inside the table', () => {
    renderEventsWith([makeEvent({ id: 1, jobId: 7 }), makeEvent({ id: 2, jobId: 8 })]);
    const table = screen.getByRole('table');
    // Two data rows plus the header row.
    expect(within(table).getAllByRole('row')).toHaveLength(3);
    expect(within(table).getAllByRole('cell')).toHaveLength(8);
  });

  it('shows the translated event label and the raw detail', () => {
    renderEventsWith([makeEvent({ event: 'search_fallback', detail: 'looser query' })]);
    expect(screen.getByText(t.event.search_fallback)).toBeInTheDocument();
    expect(screen.getByText('looser query')).toBeInTheDocument();
  });

  it('falls back to the raw code for an event the UI has no label for', () => {
    renderEventsWith([makeEvent({ event: 'some_future_event' })]);
    expect(screen.getByText('some_future_event')).toBeInTheDocument();
  });

  it('links the job column to the job detail page', () => {
    renderEventsWith([makeEvent({ jobId: 91 })]);
    const link = screen.getByRole('link', { name: '#91' });
    expect(link).toHaveAttribute('href', '/jobs/91');
  });

  it('keeps the job link a link rather than absorbing it into the cell role', () => {
    // Invisible regression: role="cell" on the <a> itself would look and click
    // identically but strip the anchor of its link role. Same guard Jobs got
    // in #206.
    renderEventsWith([makeEvent({ jobId: 91 })]);
    const link = screen.getByRole('link', { name: '#91' });
    expect(link.closest('[role="cell"]')).not.toBe(link);
    expect(link.closest('[role="cell"]')).toBeInTheDocument();
  });

  it('narrows the rendered rows to those matching the filter box', async () => {
    const user = userEvent.setup();
    renderEventsWith([
      makeEvent({ id: 1, jobId: 7, detail: 'matched the needle' }),
      makeEvent({ id: 2, jobId: 8, detail: 'something else entirely' }),
    ]);
    const table = screen.getByRole('table');
    expect(within(table).getAllByRole('row')).toHaveLength(3);

    await user.type(screen.getByPlaceholderText(t.events.filterPlaceholder), 'needle');

    expect(within(table).getAllByRole('row')).toHaveLength(2);
    expect(screen.getByText('matched the needle')).toBeInTheDocument();
    expect(screen.queryByText('something else entirely')).not.toBeInTheDocument();
  });

  it('keeps the empty state outside the table, which admits only rows', () => {
    renderEventsWith([]);
    // EmptyState wraps the message in decorative dashes ("── … ──").
    const empty = screen.getByText(new RegExp(t.events.empty));
    expect(empty.closest('[role="table"]')).toBeNull();
  });
});
