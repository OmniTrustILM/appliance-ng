import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { setCsrfToken } from '../api/client';
import { SetupFlow } from './SetupFlow';
import { stubApi } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(undefined);
});

describe('SetupFlow', () => {
  it('starts at the code page without a session', async () => {
    stubApi({ 'GET /appliance/api/v1/setup/state': [200, { step: 'code' }] });
    render(<SetupFlow />);
    expect(await screen.findByRole('heading', { name: 'Enter the setup code' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')[0]).toHaveAttribute('aria-current', 'step');
  });

  it('resumes at the step the server reports and restores the CSRF token', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/setup/state': [200, { step: 'node', csrfToken: 'tok-9' }],
      'GET /appliance/api/v1/setup/dhcp': [200, {}],
      'PUT /appliance/api/v1/setup/node': [204],
    });
    render(<SetupFlow />);
    expect(await screen.findByRole('heading', { name: 'Node settings' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')[3]).toHaveAttribute('aria-current', 'step');
    await userEvent.type(screen.getByLabelText('Host name'), 'node1.example.com');
    await userEvent.type(screen.getByLabelText('DNS servers'), '9.9.9.9');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('heading', { name: 'Review' })).toBeInTheDocument();
    const put = mock.mock.calls.find(([r]) => r.method === 'PUT')![0];
    expect(put.headers.get('X-CSRF-Token')).toBe('tok-9');
  });

  it('walks from the code to the applying page', async () => {
    stubApi({
      'GET /appliance/api/v1/setup/state': [200, { step: 'code' }],
      'POST /appliance/api/v1/setup/code': [200, { csrfToken: 'tok-1' }],
      'PUT /appliance/api/v1/setup/admin': [204],
      'POST /appliance/api/v1/setup/totp': [200, { seed: 'JBSWY3DPEHPK3PXP', otpauthUrl: 'otpauth://totp/x?secret=JBSWY3DPEHPK3PXP' }],
      'POST /appliance/api/v1/setup/totp/confirm': [204],
      'GET /appliance/api/v1/setup/dhcp': [200, { hostName: 'node1.example.com', address: '192.168.1.20/24', dns: ['192.168.1.1'] }],
      'PUT /appliance/api/v1/setup/node': [204],
      'POST /appliance/api/v1/setup/apply': [202],
    });
    render(<SetupFlow />);
    await userEvent.type(await screen.findByLabelText('Setup code'), 'abcdefghjk');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await userEvent.type(await screen.findByLabelText('User name'), 'admin');
    await userEvent.type(screen.getByLabelText('Password'), 'correct horse battery');
    await userEvent.type(screen.getByLabelText('Confirm password'), 'correct horse battery');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('JBSWY3DPEHPK3PXP');
    await userEvent.type(screen.getByLabelText('Code from the authenticator'), '123456');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByDisplayValue('node1.example.com');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('heading', { name: 'Review' })).toBeInTheDocument();
    expect(screen.getByText('admin')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(await screen.findByRole('link', { name: 'https://192.168.1.20:8443/' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')[5]).toHaveAttribute('aria-current', 'step');
  });

  it('can go back from the review to the node settings with the saved answers', async () => {
    stubApi({
      'GET /appliance/api/v1/setup/state': [200, { step: 'node', csrfToken: 't' }],
      'GET /appliance/api/v1/setup/dhcp': [200, { hostName: 'node1.example.com', dns: ['9.9.9.9'] }],
      'PUT /appliance/api/v1/setup/node': [204],
    });
    render(<SetupFlow />);
    await screen.findByDisplayValue('node1.example.com');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Change the settings' }));
    expect(await screen.findByRole('heading', { name: 'Node settings' })).toBeInTheDocument();
  });

  it('returns to the code page when the session ends', async () => {
    stubApi({
      'GET /appliance/api/v1/setup/state': [200, { step: 'admin', csrfToken: 't' }],
      'PUT /appliance/api/v1/setup/admin': [401, { code: 'unauthorized', message: 'x' }],
    });
    render(<SetupFlow />);
    await userEvent.type(await screen.findByLabelText('User name'), 'admin');
    await userEvent.type(screen.getByLabelText('Password'), 'correct horse battery');
    await userEvent.type(screen.getByLabelText('Confirm password'), 'correct horse battery');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('heading', { name: 'Enter the setup code' })).toBeInTheDocument();
  });

  it('shows the applying page after a reload that reports applying', async () => {
    stubApi({ 'GET /appliance/api/v1/setup/state': [200, { step: 'applying' }] });
    render(<SetupFlow />);
    expect(await screen.findByRole('heading', { name: 'Installing' })).toBeInTheDocument();
  });

  it('shows a generic message when the appliance cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network'); }));
    render(<SetupFlow />);
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
