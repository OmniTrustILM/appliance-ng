import { act, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { StatusPage } from './StatusPage';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const status = {
  setup: 'complete',
  addonStep: 'cert-manager',
  conditions: [
    { type: 'Platform', status: 'True', reason: 'Ready', message: 'ILM is running.' },
    { type: 'Certificate', status: 'False', reason: 'Pending', message: 'Waiting for the issuer.' },
  ],
  versions: { appliance: '1.2.0', talos: 'v1.12.0', kubernetes: 'v1.37.0', ilm: '3.0.0' },
  rebootRequired: true,
};

function renderPage() {
  return render(<MemoryRouter><StatusPage /></MemoryRouter>);
}

describe('StatusPage', () => {
  it('shows the setup state, versions, add-on step and conditions', async () => {
    stubApi({ 'GET /api/v1/status': [200, status] });
    renderPage();
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
    expect(screen.getByText('1.2.0')).toBeInTheDocument();
    expect(screen.getByText('v1.12.0')).toBeInTheDocument();
    expect(screen.getByText(/cert-manager/)).toBeInTheDocument();
    expect(screen.getByText('Waiting for the issuer.')).toBeInTheDocument();
    expect(screen.getByText('Pending')).toBeInTheDocument();
    expect(screen.getByText(/restart/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /administrator credential/i })).toHaveAttribute('href', '/wizard/done');
  });

  it('shows the node address and refresh note', async () => {
    stubApi({ 'GET /api/v1/status': [200, { ...status, nodeAddress: '10.0.0.5' }] });
    renderPage();
    expect(await screen.findByText('10.0.0.5')).toBeInTheDocument();
    expect(screen.getByText(/Refreshed every five seconds/)).toBeInTheDocument();
  });

  it('shows the setup badge', async () => {
    stubApi({ 'GET /api/v1/status': [200, status] });
    const { unmount } = renderPage();
    expect(await screen.findByText('Setup complete')).toBeInTheDocument();
    unmount();
    stubApi({ 'GET /api/v1/status': [200, { ...status, setup: 'part2' }] });
    renderPage();
    expect(await screen.findByText('Setup in progress')).toBeInTheDocument();
  });

  it('counts ready conditions', async () => {
    const conditions = [
      { type: 'A', status: 'True' },
      { type: 'B', status: 'True' },
      { type: 'C', status: 'False' },
    ];
    stubApi({ 'GET /api/v1/status': [200, { ...status, conditions }] });
    renderPage();
    expect(await screen.findByText('2 of 3 ready')).toBeInTheDocument();
  });

  it('shows a loading status before the first status', () => {
    stubApi({ 'GET /api/v1/status': [200, status] });
    renderPage();
    expect(screen.getByRole('status')).toHaveTextContent('Loading…');
  });

  it('shows an error alert when the first read fails', async () => {
    stubApi({ 'GET /api/v1/status': [500, { code: 'internal', message: 'x' }] });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });

  it('marks its reads as background refreshes, which do not keep the session alive', async () => {
    const { mock } = stubApi({ 'GET /api/v1/status': [200, status] });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(mock.mock.calls[0][0].headers.get('X-Background-Refresh')).toBe('1');
  });

  it('points to the wizard while setup is open', async () => {
    stubApi({ 'GET /api/v1/status': [200, { ...status, setup: 'part2', addonStep: undefined, rebootRequired: false }] });
    renderPage();
    expect(await screen.findByText(/not complete/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Continue the setup' })).toHaveAttribute('href', '/wizard/');
  });

  it('refreshes every five seconds and keeps the last state when a refresh fails', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    stubApi({
      'GET /api/v1/status': () => (++calls === 2 ? [500, { code: 'internal', message: 'x' }] : [200, status]),
    });
    renderPage();
    await screen.findByText('Setup is complete.');
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(calls).toBe(2);
    expect(screen.getByRole('alert')).toHaveTextContent('cannot be reached');
    expect(screen.getByText('Setup is complete.')).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('gives up a read that hangs and polls again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    vi.stubGlobal('fetch', vi.fn((request: Request) => {
      if (++calls === 1) {
        return new Promise<Response>((_, reject) => request.signal.addEventListener('abort', () => reject(request.signal.reason)));
      }
      return Promise.resolve(new Response(JSON.stringify(status), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }));
    renderPage();
    await act(() => vi.advanceTimersByTimeAsync(10000));
    expect(screen.getByRole('alert')).toHaveTextContent('cannot be reached');
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(calls).toBe(2);
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
  });

  it('stops polling when it is closed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { mock } = stubApi({ 'GET /api/v1/status': [200, status] });
    const { unmount } = renderPage();
    await screen.findByText('Setup is complete.');
    unmount();
    await act(() => vi.advanceTimersByTimeAsync(20000));
    expect(mock).toHaveBeenCalledTimes(1);
  });
});
