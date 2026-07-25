import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import SideNav from './SideNav';

const groups = [
  {
    label: 'MONITOR',
    items: [
      { to: '/', label: 'overview', end: true },
      { to: '/jobs', label: 'jobs', badge: 12 },
      { to: '/health', label: 'health', badge: 3, alert: true },
      { to: '/peers', label: 'peers', badge: 0 },
    ],
  },
];

describe('SideNav', () => {
  it('renders a badge when the count is above zero', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    expect(screen.getByText('12')).toBeInTheDocument();
  });

  it('hides a zero badge rather than drawing a 0', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    // 'peers' has badge 0; nothing in the nav should render the digit.
    expect(screen.queryByText('0')).not.toBeInTheDocument();
  });

  it('marks an alerting badge so it can be styled apart', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    expect(screen.getByText('3')).toHaveAttribute('data-alert', 'true');
  });

  it('renders each group label', () => {
    render(<MemoryRouter><SideNav groups={groups} /></MemoryRouter>);
    expect(screen.getByText('MONITOR')).toBeInTheDocument();
  });
});
