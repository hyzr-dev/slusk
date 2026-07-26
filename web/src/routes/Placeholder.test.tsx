import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Search from './Search';

describe('placeholder views', () => {
  it('search says what is missing and points at the issue', () => {
    render(<Search />);
    expect(screen.getByText(/not built yet/)).toBeInTheDocument();
    expect(screen.getByText(/#58/)).toBeInTheDocument();
  });
});
