import { act, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApplyingPage } from './ApplyingPage';
import { stubApi } from './test-utils';

const stateRoute = 'GET /appliance/api/v1/setup/state';
const readinessRoute = 'GET /appliance/api/v1/readiness';
const down = [503] as const;

const assign = vi.fn();
beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('location', { hostname: window.location.hostname, assign });
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  assign.mockReset();
});

/** Lets the clock run by seconds and the answers of the requests it started settle. */
async function wait(seconds: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(seconds * 1000);
  });
}

describe('ApplyingPage on the host the browser used', () => {
  it('shows the applying stage while the setup service answers', async () => {
    const { mock } = stubApi({ [stateRoute]: [200, { step: 'applying' }] });
    render(<ApplyingPage address={window.location.hostname} />);
    await wait(5);
    expect(mock).toHaveBeenCalledTimes(1);
    expect(screen.getByText('Applying the settings')).toBeInTheDocument();
    await wait(5);
    expect(mock).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.queryByText(/address shown on the appliance console/)).not.toBeInTheDocument();
  });

  it('points to the appliance console when the page does not know the address, as after a reload', () => {
    stubApi({ [stateRoute]: [200, { step: 'applying' }] });
    render(<ApplyingPage />);
    expect(screen.getByText(/If the address changed, open the console at the address shown on the appliance console\./)).toBeInTheDocument();
  });

  it('shows the install stage once the requests fail', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network'); }));
    render(<ApplyingPage />);
    await wait(5);
    expect(screen.getByText('Installing and restarting')).toBeInTheDocument();
  });

  it('shows the start stage while the console is not ready to sign in', async () => {
    stubApi({ [stateRoute]: down, [readinessRoute]: [200, { signIn: false }] });
    render(<ApplyingPage />);
    await wait(5);
    expect(screen.getByText('Starting the appliance')).toBeInTheDocument();
    expect(assign).not.toHaveBeenCalled();
  });

  it('opens the console once signing in works', async () => {
    stubApi({ [stateRoute]: down, [readinessRoute]: [200, { signIn: true }] });
    render(<ApplyingPage />);
    await wait(5);
    expect(assign).toHaveBeenCalledWith('/appliance/');
  });

  it('asks every five seconds and stops when the page is left', async () => {
    const { mock } = stubApi({ [stateRoute]: [200, { step: 'applying' }] });
    const { unmount } = render(<ApplyingPage />);
    await wait(4);
    expect(mock).not.toHaveBeenCalled();
    await wait(1);
    expect(mock).toHaveBeenCalledTimes(1);
    unmount();
    await wait(30);
    expect(mock).toHaveBeenCalledTimes(1);
  });

  it('does not start a poll while the previous one is still waiting for an answer', async () => {
    const mock = vi.fn(() => new Promise<Response>(() => {}));
    vi.stubGlobal('fetch', mock);
    render(<ApplyingPage />);
    await wait(5);
    expect(mock).toHaveBeenCalledTimes(1);
    await wait(30);
    expect(mock).toHaveBeenCalledTimes(1);
  });

  it('shows the elapsed time as minutes and seconds', async () => {
    stubApi({ [stateRoute]: [200, { step: 'applying' }] });
    render(<ApplyingPage />);
    expect(screen.getByText('0:00')).toBeInTheDocument();
    await wait(65);
    expect(screen.getByText('1:05')).toBeInTheDocument();
  });

  it('counts the time on the clock, not the ticks a throttled tab delivered', async () => {
    stubApi({ [stateRoute]: [200, { step: 'applying' }] });
    render(<ApplyingPage />);
    act(() => {
      vi.setSystemTime(Date.now() + 90_000);
    });
    await wait(1);
    expect(screen.getByText('1:31')).toBeInTheDocument();
  });
});

describe('ApplyingPage on another address than the browser used', () => {
  it('sends no request to the other host and shows the link and the elapsed time', async () => {
    const { mock } = stubApi({});
    render(<ApplyingPage address="10.0.0.5" />);
    await wait(125);
    expect(mock).not.toHaveBeenCalled();
    expect(screen.getByRole('link', { name: 'Open the console' })).toHaveAttribute('href', 'https://10.0.0.5:8443/');
    expect(screen.getByText('2:05')).toBeInTheDocument();
    expect(screen.getByText('10.0.0.5')).toBeInTheDocument();
  });
});
