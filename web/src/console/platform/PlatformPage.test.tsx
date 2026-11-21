import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { PlatformPage } from './PlatformPage';

const component = (name: string, over: object = {}) => ({
  name, title: name[0].toUpperCase() + name.slice(1), private: false, chosen: false, needsCredentials: false, needsMail: false, ...over,
});
const platform = (components: object[], over: object = {}) =>
  [200, { ilmVersion: '2.17.0', hostName: 'ilm.example.test', components, ...over }] as const;

const open = () => render(<MemoryRouter><PlatformPage /></MemoryRouter>);

afterEach(() => vi.unstubAllGlobals());

describe('PlatformPage', () => {
  it('shows the version read-only and saves the host name and the toggled components', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': platform([component('scanner', { chosen: true }), component('discovery')]),
      'PUT /appliance/api/v1/platform': [202],
    });
    open();
    expect(await screen.findByLabelText('ILM version')).toHaveValue('2.17.0');
    expect(screen.getByLabelText('ILM version')).toHaveAttribute('readonly');
    expect(screen.getByRole('checkbox', { name: 'Scanner' })).toBeChecked();
    await userEvent.click(screen.getByRole('checkbox', { name: 'Discovery' }));
    await userEvent.click(screen.getByRole('checkbox', { name: 'Scanner' }));
    await userEvent.clear(screen.getByLabelText('ILM host name'));
    await userEvent.type(screen.getByLabelText('ILM host name'), 'new.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'dashboard' })).toHaveAttribute('href', '/');
    expect(sent['PUT /appliance/api/v1/platform']).toEqual([{ hostName: 'new.example.test', components: ['discovery'] }]);
  });

  it('disables a private component that needs credentials and links to the registries', async () => {
    stubApi({
      'GET /appliance/api/v1/platform': platform([
        component('vault', { private: true, needsCredentials: true }),
        component('scanner', { private: true, chosen: true }),
      ]),
    });
    open();
    expect(await screen.findByRole('checkbox', { name: /Vault/ })).toBeDisabled();
    expect(screen.getByText('Needs registry credentials')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /registry credentials/i })).toHaveAttribute('href', '/registries');
    expect(screen.getByRole('checkbox', { name: 'Scanner' })).toBeEnabled();
  });

  it('keeps a chosen component that lost its credentials enabled, so it can be removed', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': platform([component('vault', { private: true, chosen: true, needsCredentials: true })]),
      'PUT /appliance/api/v1/platform': [202],
    });
    open();
    const vault = await screen.findByRole('checkbox', { name: /Vault/ });
    expect(vault).toBeEnabled();
    expect(vault).toHaveAccessibleName(/or remove it from the components/);
    await userEvent.click(vault);
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/platform']).toEqual([{ hostName: 'ilm.example.test', components: [] }]);
  });

  it('describes only the components that need something', async () => {
    stubApi({ 'GET /appliance/api/v1/platform': platform([component('scanner')]) });
    open();
    const row = (await screen.findByRole('checkbox', { name: 'Scanner' })).closest('label');
    expect(row?.querySelector('.text-content-muted')).toBeNull();
  });

  it('keeps the saved choice on screen until the new state is loaded', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/platform': platform([component('scanner'), component('discovery')]),
      'PUT /appliance/api/v1/platform': [202],
    });
    open();
    await userEvent.click(await screen.findByRole('checkbox', { name: 'Discovery' }));
    const answer = mock.getMockImplementation();
    mock.mockImplementationOnce(answer!).mockImplementationOnce(() => new Promise<Response>(() => undefined));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Discovery' })).toBeChecked();
  });

  it('hints at the mail page for a component that needs mail', async () => {
    stubApi({ 'GET /appliance/api/v1/platform': platform([component('email', { needsMail: true })]) });
    open();
    expect(await screen.findByRole('link', { name: /mail server/i })).toHaveAttribute('href', '/mail');
    expect(screen.getByRole('checkbox', { name: /Email/ })).toBeEnabled();
  });

  it('shows a refused component and a refused host name at their place', async () => {
    stubApi({
      'GET /appliance/api/v1/platform': platform([component('email')]),
      'PUT /appliance/api/v1/platform': (body) => (body as { hostName: string }).hostName === 'bad'
        ? [400, { code: 'invalid', field: 'hostName', message: 'Name is not valid.' }]
        : [400, { code: 'invalid', field: 'components', message: 'The component "email" sends mail and needs the mail server settings.' }],
    });
    open();
    await userEvent.click(await screen.findByRole('checkbox', { name: 'Email' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('"email" sends mail');
    await userEvent.clear(screen.getByLabelText('ILM host name'));
    await userEvent.type(screen.getByLabelText('ILM host name'), 'bad');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Name is not valid.')).toBeInTheDocument();
    expect(screen.getByLabelText('ILM host name')).toHaveAccessibleDescription('Name is not valid.');
    expect(screen.queryByText(/sends mail and needs/)).not.toBeInTheDocument();
  });

  it('reports a page that cannot be loaded and a save that cannot reach the appliance', async () => {
    stubApi({ 'GET /appliance/api/v1/platform': [409, { code: 'conflict', message: 'Setup is not done.' }] });
    const first = open();
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup is not done.');
    first.unmount();
    const { mock } = stubApi({ 'GET /appliance/api/v1/platform': platform([]) });
    open();
    await screen.findByLabelText('ILM version');
    mock.mockRejectedValueOnce(new TypeError('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
