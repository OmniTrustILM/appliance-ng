import { StrictMode } from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TotpPage } from './TotpPage';
import { stubApi } from './test-utils';

const enrolment = { seed: 'JBSWY3DPEHPK3PXP', otpauthUrl: 'otpauth://totp/ILM:admin?secret=JBSWY3DPEHPK3PXP&issuer=ILM' };

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('TotpPage', () => {
  it('shows the QR code and the secret under its name, and keeps neither', async () => {
    const log = vi.spyOn(console, 'log');
    stubApi({ 'POST /appliance/api/v1/setup/totp': [200, enrolment] });
    render(<TotpPage onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(await screen.findByRole('textbox', { name: 'Secret' })).toHaveValue('JBSWY3DPEHPK3PXP');
    expect(screen.getByRole('textbox', { name: 'Secret' })).toHaveAttribute('readonly');
    expect(screen.getByRole('img', { name: /QR code/ })).toBeInTheDocument();
    expect(localStorage.length + sessionStorage.length).toBe(0);
    expect(location.href).not.toContain('JBSWY3DPEHPK3PXP');
    expect(log).not.toHaveBeenCalled();
  });

  it('asks for one enrolment only when StrictMode runs the effect twice', async () => {
    const { sent } = stubApi({ 'POST /appliance/api/v1/setup/totp': [200, enrolment] });
    render(
      <StrictMode>
        <TotpPage onDone={vi.fn()} onExpired={vi.fn()} />
      </StrictMode>,
    );
    expect(await screen.findByRole('textbox', { name: 'Secret' })).toHaveValue('JBSWY3DPEHPK3PXP');
    expect(sent['POST /appliance/api/v1/setup/totp']).toHaveLength(1);
  });

  it('confirms the code and moves on', async () => {
    const { sent } = stubApi({
      'POST /appliance/api/v1/setup/totp': [200, enrolment],
      'POST /appliance/api/v1/setup/totp/confirm': [204],
    });
    const onDone = vi.fn();
    render(<TotpPage onDone={onDone} onExpired={vi.fn()} />);
    await screen.findByDisplayValue('JBSWY3DPEHPK3PXP');
    await userEvent.type(screen.getByLabelText('Code from the authenticator'), '123456');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent['POST /appliance/api/v1/setup/totp/confirm']).toEqual([{ code: '123456' }]);
  });

  it('shows the appliance time after a refused code', async () => {
    stubApi({
      'POST /appliance/api/v1/setup/totp': [200, enrolment],
      'POST /appliance/api/v1/setup/totp/confirm': [400, { code: 'invalid_code', message: 'x', serverTime: '2026-11-18T10:00:00Z' }],
    });
    render(<TotpPage onDone={vi.fn()} onExpired={vi.fn()} />);
    await screen.findByDisplayValue('JBSWY3DPEHPK3PXP');
    await userEvent.type(screen.getByLabelText('Code from the authenticator'), '123456');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByText(/Appliance time: .*2026/)).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('not accepted');
  });

  it('rejects a code that is not six digits without a request', async () => {
    const { mock } = stubApi({ 'POST /appliance/api/v1/setup/totp': [200, enrolment] });
    render(<TotpPage onDone={vi.fn()} onExpired={vi.fn()} />);
    await screen.findByDisplayValue('JBSWY3DPEHPK3PXP');
    await userEvent.type(screen.getByLabelText('Code from the authenticator'), '12');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(screen.getByLabelText('Code from the authenticator')).toHaveAccessibleDescription(/6 digits/);
    expect(mock).toHaveBeenCalledTimes(1);
  });

  it('shows a generic message when the enrolment cannot be started', async () => {
    stubApi({ 'POST /appliance/api/v1/setup/totp': [409, { code: 'conflict', message: 'x' }] });
    render(<TotpPage onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(await screen.findByRole('alert')).toHaveTextContent('Reload the page');
  });
});
