import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { FlashProvider, useFlash } from './FlashContext';
import StatusBar from './StatusBar';

function Trigger() {
  const flash = useFlash();
  return <button onClick={() => flash('cancelled #2291')}>fire</button>;
}

describe('StatusBar', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('shows a flash message and clears it again', () => {
    render(
      <FlashProvider>
        <Trigger />
        <StatusBar />
      </FlashProvider>,
    );

    expect(screen.queryByText(/cancelled/)).not.toBeInTheDocument();

    act(() => { screen.getByText('fire').click(); });
    expect(screen.getByText(/cancelled #2291/)).toBeInTheDocument();

    act(() => { vi.advanceTimersByTime(3300); });
    expect(screen.queryByText(/cancelled/)).not.toBeInTheDocument();
  });
});
