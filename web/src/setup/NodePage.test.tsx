import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { NodePage } from './NodePage';
import { stubApi } from './test-utils';

const dhcp = {
  hostName: 'node1.example.com',
  address: '192.168.1.20/24',
  gateway: '192.168.1.1',
  dns: ['192.168.1.1'],
  ntp: ['time.example.com'],
};

afterEach(() => vi.unstubAllGlobals());

async function loaded(routes: Parameters<typeof stubApi>[0] = {}, props: Partial<Parameters<typeof NodePage>[0]> = {}) {
  const api = stubApi({ 'GET /appliance/api/v1/setup/dhcp': [200, dhcp], ...routes });
  const onDone = vi.fn();
  render(<NodePage onDone={onDone} onExpired={vi.fn()} {...props} />);
  await screen.findByDisplayValue('node1.example.com');
  return { ...api, onDone };
}

describe('NodePage', () => {
  it('prefills the fields from DHCP and warns that the address must be reserved', async () => {
    await loaded();
    expect(screen.getByLabelText('DNS servers')).toHaveValue('192.168.1.1');
    expect(screen.getByLabelText('NTP servers')).toHaveValue('time.example.com');
    expect(screen.getByLabelText('Keep the address from DHCP')).toBeChecked();
    expect(screen.getByRole('status')).toHaveTextContent(/reserve/i);
  });

  it('sends empty address and gateway when DHCP is kept', async () => {
    const { sent, onDone } = await loaded({ 'PUT /appliance/api/v1/setup/node': [204] });
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent['PUT /appliance/api/v1/setup/node']).toEqual([{
      hostName: 'node1.example.com',
      address: '',
      gateway: '',
      dns: ['192.168.1.1'],
      ntp: ['time.example.com'],
    }]);
    expect(onDone.mock.calls[0][1]).toBe('192.168.1.20');
  });

  it('sends a static address with the gateway, the proxy and lower-cased names', async () => {
    const { sent, onDone } = await loaded({ 'PUT /appliance/api/v1/setup/node': [204] });
    await userEvent.click(screen.getByLabelText('Use a static address'));
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    const address = screen.getByLabelText('Address (CIDR)');
    await userEvent.clear(address);
    await userEvent.type(address, '10.0.0.5/24');
    const gateway = screen.getByLabelText('Gateway');
    await userEvent.clear(gateway);
    await userEvent.type(gateway, '10.0.0.1');
    const host = screen.getByLabelText('Host name');
    await userEvent.clear(host);
    await userEvent.type(host, 'Node2.Example.com');
    await userEvent.clear(screen.getByLabelText('DNS servers'));
    await userEvent.type(screen.getByLabelText('DNS servers'), '9.9.9.9, 1.1.1.1');
    await userEvent.type(screen.getByLabelText('HTTP proxy'), 'http://proxy.example.com:3128');
    await userEvent.type(screen.getByLabelText('Hosts that bypass the proxy'), '.example.com');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent['PUT /appliance/api/v1/setup/node']).toEqual([{
      hostName: 'node2.example.com',
      address: '10.0.0.5/24',
      gateway: '10.0.0.1',
      dns: ['9.9.9.9', '1.1.1.1'],
      ntp: ['time.example.com'],
      proxy: { http: 'http://proxy.example.com:3128', noProxy: '.example.com' },
    }]);
    expect(onDone.mock.calls[0][1]).toBe('10.0.0.5');
  });

  it('validates the fields', async () => {
    const { mock } = await loaded();
    const host = screen.getByLabelText('Host name');
    await userEvent.clear(host);
    await userEvent.type(host, 'node1');
    await userEvent.clear(screen.getByLabelText('DNS servers'));
    await userEvent.click(screen.getByLabelText('Use a static address'));
    const address = screen.getByLabelText('Address (CIDR)');
    await userEvent.clear(address);
    await userEvent.type(address, '10.0.0.5');
    await userEvent.clear(screen.getByLabelText('Gateway'));
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(host).toHaveAccessibleDescription(/fully qualified/);
    expect(address).toHaveAccessibleDescription(/CIDR/);
    expect(screen.getByLabelText('Gateway')).toHaveAccessibleDescription(/IPv4/);
    expect(screen.getByLabelText('DNS servers')).toHaveAccessibleDescription(/at least one/i);
    expect(mock.mock.calls.filter(([r]) => r.method === 'PUT')).toHaveLength(0);
  });

  it('shows a server error at its field', async () => {
    await loaded({ 'PUT /appliance/api/v1/setup/node': [400, { code: 'invalid', field: 'dns', message: 'A DNS server is not reachable.' }] });
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByText('A DNS server is not reachable.')).toBeInTheDocument();
  });

  it('starts empty when the node cannot report DHCP values', async () => {
    stubApi({ 'GET /appliance/api/v1/setup/dhcp': [200, {}] });
    render(<NodePage onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(await screen.findByLabelText('Host name')).toHaveValue('');
    expect(screen.getByLabelText('DNS servers')).toHaveValue('');
  });

  it('shows the saved answers again when it is reopened from the review', async () => {
    const saved = { hostName: 'saved.example.com', address: '', gateway: '', dns: ['9.9.9.9'], ntp: [] };
    stubApi({ 'GET /appliance/api/v1/setup/dhcp': [200, dhcp] });
    render(<NodePage initial={saved} onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(await screen.findByDisplayValue('saved.example.com')).toBeInTheDocument();
    expect(screen.getByLabelText('DNS servers')).toHaveValue('9.9.9.9');
  });
});
