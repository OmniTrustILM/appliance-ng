import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { NodePage } from './NodePage';

afterEach(() => vi.unstubAllGlobals());

const appliance = {
  node: { hostName: 'node1.example.com', address: '10.0.0.5/24', gateway: '10.0.0.1' },
  dns: ['10.0.0.2'],
  ntp: ['time.example.com'],
  proxy: { http: 'http://proxy.example.com:3128' },
  tlsSource: 'builtin',
  trustSet: 'default',
  registries: [],
};

async function loaded(routes: Parameters<typeof stubApi>[0] = {}, settings: object = appliance) {
  const api = stubApi({ 'GET /appliance/api/v1/appliance': [200, settings], ...routes });
  render(<MemoryRouter><NodePage /></MemoryRouter>);
  await screen.findByDisplayValue('node1.example.com');
  return api;
}

describe('console NodePage', () => {
  it('prefills the form and shows the host name and address read-only', async () => {
    await loaded();
    expect(screen.getByLabelText('Gateway')).toHaveValue('10.0.0.1');
    expect(screen.getByLabelText('DNS servers')).toHaveValue('10.0.0.2');
    expect(screen.getByLabelText('NTP servers')).toHaveValue('time.example.com');
    expect(screen.getByLabelText('HTTP proxy')).toHaveValue('http://proxy.example.com:3128');
    expect(screen.getByLabelText('Address')).toHaveAttribute('readonly');
    expect(screen.getByText('The address is fixed after the setup.')).toBeInTheDocument();
    const host = screen.getByLabelText('Host name');
    expect(host).toHaveAttribute('readonly');
    expect(host).toHaveAccessibleDescription(/host name is fixed after the setup/i);
    expect(screen.getByRole('link', { name: 'ILM host name' })).toHaveAttribute('href', '/platform');
    expect(screen.getByRole('link', { name: 'certificate' })).toHaveAttribute('href', '/certificate');
  });

  it('names the DHCP address when the spec has none and offers no gateway', async () => {
    await loaded({}, { ...appliance, node: { hostName: 'node1.example.com' } });
    expect(screen.getByLabelText('Address')).toHaveValue('Assigned by DHCP');
    expect(screen.getByLabelText('Gateway')).toBeDisabled();
  });

  it('shows a field error without a field of its own as a form message', async () => {
    await loaded({ 'PUT /appliance/api/v1/node': [400, { code: 'invalid', message: 'The node address is fixed at setup.', field: 'address' }] });
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('The node address is fixed at setup.');
  });

  it('saves without the host name and address and without a restart hint when the proxy is unchanged', async () => {
    const { sent } = await loaded({ 'PUT /appliance/api/v1/node': [202] });
    const dns = screen.getByLabelText('DNS servers');
    await userEvent.clear(dns);
    await userEvent.type(dns, '10.0.0.3, 10.0.0.4');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/saved/i)).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/node']).toEqual([{
      gateway: '10.0.0.1',
      dns: ['10.0.0.3', '10.0.0.4'],
      ntp: ['time.example.com'],
      proxy: { http: 'http://proxy.example.com:3128' },
    }]);
    expect(screen.queryByRole('link', { name: /restart/i })).not.toBeInTheDocument();
  });

  it('points to the System page when the proxy changed', async () => {
    await loaded({ 'PUT /appliance/api/v1/node': [202] });
    const https = screen.getByLabelText('HTTPS proxy');
    await userEvent.type(https, 'https://proxy.example.com:3129');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('link', { name: /restart the node/i })).toHaveAttribute('href', '/system');
  });

  it('shows a field error from a 400 response', async () => {
    await loaded({ 'PUT /appliance/api/v1/node': [400, { code: 'invalid', message: 'A gateway needs a static address.', field: 'gateway' }] });
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('A gateway needs a static address.')).toBeInTheDocument();
    expect(screen.getByLabelText('Gateway')).toBeInvalid();
  });

  it('maps a proxy field error to the proxy group', async () => {
    await loaded({ 'PUT /appliance/api/v1/node': [400, { code: 'invalid', message: 'Bad proxy.', field: 'noProxy' }] });
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Bad proxy.')).toBeInTheDocument();
    expect(screen.getByLabelText('HTTP proxy')).toBeInvalid();
  });

  it('shows other failures as a form message', async () => {
    await loaded({ 'PUT /appliance/api/v1/node': [409, { code: 'conflict', message: 'The setup is not complete.' }] });
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('The setup is not complete.');
  });

  it('validates before sending', async () => {
    const { sent } = await loaded({ 'PUT /appliance/api/v1/node': [202] });
    const gateway = screen.getByLabelText('Gateway');
    await userEvent.clear(gateway);
    await userEvent.type(gateway, 'nope');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByLabelText('Gateway')).toBeInvalid();
    expect(sent['PUT /appliance/api/v1/node']).toBeUndefined();
  });

  it('says so when the appliance cannot be read', async () => {
    stubApi({});
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('down')));
    render(<MemoryRouter><NodePage /></MemoryRouter>);
    expect(await screen.findByRole('alert')).toHaveTextContent(/cannot be reached/i);
  });

  it('says so when saving cannot reach the appliance', async () => {
    const { mock } = await loaded({ 'PUT /appliance/api/v1/node': [202] });
    mock.mockRejectedValueOnce(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/cannot be reached/i);
  });
});
