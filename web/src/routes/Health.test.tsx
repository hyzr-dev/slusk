import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/queries';
import type { ModuleStatus, StatusReport } from '../api/types';
import { t } from '../strings';
import Health from './Health';

function makeModule(overrides: Partial<ModuleStatus> = {}): ModuleStatus {
  return {
    lastAttempt: '',
    lastCompleted: '',
    lastSuccess: '',
    lastErrorAt: '',
    lastError: '',
    consecutiveFailures: 0,
    staleDeadline: '',
    live: true,
    ready: true,
    ...overrides,
  };
}

function renderHealth(moduleDetails: StatusReport['moduleDetails']) {
  const queryClient = new QueryClient();
  queryClient.setQueryData(queryKeys.status, {
    queued: 0,
    active: 0,
    stalled: 0,
    orphaned: 0,
    modules: {},
    moduleDetails,
  } satisfies StatusReport);
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Health />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Health module states', () => {
  it('shows the never-run label for a module with no lastAttempt, not a formatted date', () => {
    renderHealth({ importer: makeModule({ lastAttempt: '' }) });
    expect(screen.getByText(t.health.neverRun)).toBeInTheDocument();
  });

  it('renders a ready module without the unhealthy marker or a failure count', () => {
    renderHealth({ importer: makeModule({ lastAttempt: '2026-07-20T10:00:00Z', ready: true }) });
    const cell = screen.getByTitle('');
    expect(cell.className).not.toMatch(/unhealthy/);
    expect(cell.textContent).not.toContain('consecutive');
  });

  it('marks a not-ready module unhealthy and appends its consecutive failure count independently', () => {
    renderHealth({
      importer: makeModule({
        lastAttempt: '2026-07-20T10:00:00Z',
        ready: false,
        consecutiveFailures: 3,
        lastError: 'boom',
      }),
    });
    const cell = screen.getByTitle('boom');
    expect(cell.className).toMatch(/unhealthy/);
    expect(cell.textContent).toContain(t.health.consecutiveFailures(3));
  });
});
