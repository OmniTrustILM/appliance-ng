import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApplyingPage } from './ApplyingPage';

afterEach(() => vi.unstubAllGlobals());

describe('ApplyingPage', () => {
  it('tells the user where to reload and sends no request', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    vi.useFakeTimers();
    render(<ApplyingPage address="10.0.0.5" />);
    expect(screen.getByText(/installs and reboots/)).toBeInTheDocument();
    expect(screen.getByText(/five minutes/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'https://10.0.0.5:8443/' })).toHaveAttribute('href', 'https://10.0.0.5:8443/');
    vi.advanceTimersByTime(600_000);
    expect(fetchMock).not.toHaveBeenCalled();
    vi.useRealTimers();
  });

  it('points to the appliance console when the address is not known', () => {
    render(<ApplyingPage />);
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.getByText(/address shown on the appliance console/)).toBeInTheDocument();
  });
});
