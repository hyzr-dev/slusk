import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Search from './Search';
import Chat from './Chat';

describe('placeholder views', () => {
  it('search says what is missing and points at the issue', () => {
    render(<Search />);
    expect(screen.getByText(/not built yet/)).toBeInTheDocument();
    expect(screen.getByText(/#58/)).toBeInTheDocument();
  });

  it('chat says what is missing and points at the issue', () => {
    render(<Chat />);
    expect(screen.getByText(/no HTTP surface/)).toBeInTheDocument();
    expect(screen.getByText(/#183/)).toBeInTheDocument();
  });
});
