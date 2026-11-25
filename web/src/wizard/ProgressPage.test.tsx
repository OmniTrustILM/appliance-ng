import { act, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../setup/test-utils';
import { ProgressPage } from './ProgressPage';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const base = { setup: 'part2', conditions: [], versions: {}, rebootRequired: false };

describe('ProgressPage', () => {
  it('shows the add-on step and the conditions with their reasons and messages', async () => {
    stubApi({
      'GET /appliance/api/v1/status': [200, {
        ...base,
        addonStep: 'cert-manager',
        conditions: [
          { type: 'PlatformReady', status: 'False', reason: 'Installing', message: 'The platform is being installed.' },
          { type: 'CertificateReady', status: 'False', reason: 'IssuerFailed', message: 'The issuer refused the request.' },
          { type: 'AddonsReady', status: 'False', reason: 'Failed', message: 'cert-manager: the chart failed to install.' },
        ],
      }],
    });
    render(<ProgressPage onBack={vi.fn()} onComplete={vi.fn()} />);
    expect(await screen.findByText(/Installing add-on/)).toBeInTheDocument();
    expect(screen.getByText('The platform is being installed.')).toBeInTheDocument();
    expect(screen.getByText('IssuerFailed')).toBeInTheDocument();
    expect(screen.getByText('The issuer refused the request.')).toBeInTheDocument();
    expect(screen.getByText('cert-manager: the chart failed to install.')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /root certificate/i })).not.toBeInTheDocument();
  });

  it('offers the root certificate once the certificate is issued', async () => {
    stubApi({
      'GET /appliance/api/v1/status': [200, { ...base, conditions: [{ type: 'CertificateReady', status: 'True', reason: 'Ready' }] }],
    });
    render(<ProgressPage onBack={vi.fn()} onComplete={vi.fn()} />);
    expect(await screen.findByRole('link', { name: /root certificate/i })).toHaveAttribute('href', '/appliance/api/v1/ca.pem');
  });

  it('says the console certificate changes when the setup is complete', async () => {
    stubApi({
      'GET /appliance/api/v1/status': [200, { ...base, conditions: [{ type: 'CertificateReady', status: 'True', reason: 'Ready' }] }],
    });
    render(<ProgressPage onBack={vi.fn()} onComplete={vi.fn()} />);
    expect(await screen.findByText(/When the setup is complete, the console switches from its self-signed certificate/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /root certificate/i })).toBeInTheDocument();
  });

  it('explains a failed poll after the switch: reload and accept the new certificate', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    const { mock } = stubApi({
      'GET /appliance/api/v1/status': () => {
        calls++;
        return [200, { ...base, conditions: [{ type: 'CertificateReady', status: 'True', reason: 'Ready' }] }];
      },
    });
    render(<ProgressPage onBack={vi.fn()} onComplete={vi.fn()} />);
    await screen.findByRole('link', { name: /root certificate/i });
    expect(screen.queryByText(/Reload the page and accept/)).not.toBeInTheDocument();
    mock.mockRejectedValue(new TypeError('certificate changed'));
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(await screen.findByText(/Reload the page and accept the new certificate/)).toBeInTheDocument();
    expect(calls).toBeGreaterThan(0);
  });

  it('waits while there are no conditions yet', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, base] });
    render(<ProgressPage onBack={vi.fn()} onComplete={vi.fn()} />);
    expect(await screen.findByText(/not reported anything yet/i)).toBeInTheDocument();
  });

  it('polls every five seconds and finishes when the setup is complete', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    stubApi({ 'GET /appliance/api/v1/status': () => [200, { ...base, setup: ++calls < 2 ? 'part2' : 'complete' }] });
    const onComplete = vi.fn();
    render(<ProgressPage onBack={vi.fn()} onComplete={onComplete} />);
    await vi.waitFor(() => expect(calls).toBe(1));
    expect(onComplete).not.toHaveBeenCalled();
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(onComplete).toHaveBeenCalled();
  });

  it('shows an error when the appliance cannot be reached', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [500, { code: 'internal', message: 'x' }] });
    render(<ProgressPage onBack={vi.fn()} onComplete={vi.fn()} />);
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
