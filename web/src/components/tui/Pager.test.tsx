import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { t } from '../../strings';
import Pager, { paginationItems } from './Pager';

// Moved from Jobs.tsx alongside the component (#425) — the jobs-list pager
// was the only mount of `paginationItems`, but its behaviour is Jobs-agnostic
// and belongs with the shared control now, not with one caller's route file.
describe('paginationItems', () => {
  it('lists every page without an ellipsis when the collection is small', () => {
    expect(paginationItems(0, 7)).toEqual([0, 1, 2, 3, 4, 5, 6]);
  });

  it('always exposes the first and last page, with an ellipsis over the gap', () => {
    expect(paginationItems(5, 20)).toEqual([0, 'ellipsis', 4, 5, 6, 'ellipsis', 19]);
  });

  it('collapses the gap into a run of numbers once the neighbourhood touches a boundary', () => {
    expect(paginationItems(0, 20)).toEqual([0, 1, 'ellipsis', 19]);
    expect(paginationItems(19, 20)).toEqual([0, 'ellipsis', 18, 19]);
  });
});

describe('Pager', () => {
  it('renders the compact page list and prev/next controls, disabling out-of-range moves', () => {
    render(<Pager page={0} totalPages={20} onChange={() => {}} />);

    expect(screen.getByRole('button', { name: t.pager.previousPage })).toBeDisabled();
    expect(screen.getByRole('button', { name: t.pager.nextPage })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.pager.pageLabel(1) })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('button', { name: t.pager.pageLabel(20) })).toBeInTheDocument();
    expect(screen.getByText('…')).toBeInTheDocument();
  });

  it('calls onChange with the target page for prev, next and numbered clicks, never on a disabled control', () => {
    const onChange = vi.fn();
    render(<Pager page={5} totalPages={20} onChange={onChange} />);

    fireEvent.click(screen.getByRole('button', { name: t.pager.previousPage }));
    expect(onChange).toHaveBeenLastCalledWith(4);
    fireEvent.click(screen.getByRole('button', { name: t.pager.nextPage }));
    expect(onChange).toHaveBeenLastCalledWith(6);
    fireEvent.click(screen.getByRole('button', { name: t.pager.pageLabel(1) }));
    expect(onChange).toHaveBeenLastCalledWith(0);
    expect(onChange).toHaveBeenCalledTimes(3);
  });

  it('disables next on the last page', () => {
    render(<Pager page={19} totalPages={20} onChange={() => {}} />);
    expect(screen.getByRole('button', { name: t.pager.nextPage })).toBeDisabled();
  });
});
