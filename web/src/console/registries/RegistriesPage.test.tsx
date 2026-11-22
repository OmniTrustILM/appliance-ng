import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { RegistriesPage } from './RegistriesPage';

const list = (items: object[]) => [200, { items }] as const;
const registry = { host: 'registry.example.test', username: 'puller', passwordSet: true };

afterEach(() => vi.unstubAllGlobals());

describe('RegistriesPage', () => {
  it('lists registries with their user name and whether a password is set, never a password', async () => {
    stubApi({
      'GET /appliance/api/v1/registries': list([registry, { host: 'open.example.test', username: '', passwordSet: false }]),
    });
    render(<RegistriesPage />);
    expect(await screen.findByText('registry.example.test')).toBeInTheDocument();
    expect(screen.getByText('User name: puller')).toBeInTheDocument();
    expect(screen.getByText('Password set')).toBeInTheDocument();
    expect(screen.getByText('User name: none')).toBeInTheDocument();
    expect(screen.getByText('No password')).toBeInTheDocument();
  });

  it('says when there are no registries and reports a list that cannot be read', async () => {
    stubApi({ 'GET /appliance/api/v1/registries': list([]) });
    const first = render(<RegistriesPage />);
    expect(await screen.findByText(/no registries have credentials/i)).toBeInTheDocument();
    first.unmount();
    stubApi({ 'GET /appliance/api/v1/registries': [409, { code: 'conflict', message: 'Setup is not done.' }] });
    render(<RegistriesPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup is not done.');
  });

  it('adds a registry with a password and needs no restart', async () => {
    let items: object[] = [];
    const { sent } = stubApi({
      'GET /appliance/api/v1/registries': () => list(items),
      'PUT /appliance/api/v1/registries/registry.example.test': () => {
        items = [registry];
        return [202];
      },
    });
    render(<RegistriesPage />);
    await screen.findByText(/no registries/i);
    const submit = screen.getByRole('button', { name: 'Add the registry' });
    expect(submit).toBeDisabled();
    expect(screen.queryByText(/leave empty/i)).not.toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Registry host'), ' registry.example.test ');
    await userEvent.type(screen.getByLabelText('User name'), 'puller');
    await userEvent.type(screen.getByLabelText('Password'), 's3cret');
    await userEvent.click(submit);
    expect(await screen.findByText(/no restart is needed/i)).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/registries/registry.example.test']).toEqual([{ username: 'puller', password: 's3cret' }]);
    expect(await screen.findByText('Password set')).toBeInTheDocument();
    expect(screen.getByLabelText('Registry host')).toHaveValue('');
  });

  it('edits a registry and keeps its password when the field stays empty', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/registries': list([registry]),
      'PUT /appliance/api/v1/registries/registry.example.test': [202],
    });
    render(<RegistriesPage />);
    await userEvent.click(await screen.findByRole('button', { name: /Edit registry.example.test/ }));
    expect(screen.getByRole('heading', { name: 'Edit registry.example.test' })).toBeInTheDocument();
    expect(screen.getByLabelText('Registry host')).toHaveAttribute('readonly');
    expect(screen.getByLabelText('User name')).toHaveValue('puller');
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(screen.getByText('Leave empty to keep the current password.')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Save the credentials' }));
    expect(await screen.findByText(/no restart is needed/i)).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/registries/registry.example.test']).toEqual([{ username: 'puller' }]);
    expect(screen.getByRole('heading', { name: 'Add a registry' })).toBeInTheDocument();
  });

  it('cancels an edit', async () => {
    stubApi({ 'GET /appliance/api/v1/registries': list([registry]) });
    render(<RegistriesPage />);
    await userEvent.click(await screen.findByRole('button', { name: /Edit/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByRole('heading', { name: 'Add a registry' })).toBeInTheDocument();
  });

  it('shows the field error of a username change without a password at the password field', async () => {
    stubApi({
      'GET /appliance/api/v1/registries': list([registry]),
      'PUT /appliance/api/v1/registries/registry.example.test': [400, { code: 'invalid', message: 'A new user name needs a password.', field: 'password' }],
    });
    render(<RegistriesPage />);
    await userEvent.click(await screen.findByRole('button', { name: /Edit/ }));
    const user = screen.getByLabelText('User name');
    await userEvent.clear(user);
    await userEvent.type(user, 'other');
    await userEvent.click(screen.getByRole('button', { name: 'Save the credentials' }));
    const message = await screen.findByText('A new user name needs a password.');
    expect(message).toHaveAttribute('role', 'alert');
    expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.queryByText(/no restart/i)).not.toBeInTheDocument();
  });

  it('shows other refusals and an unreachable appliance above the form', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/registries': list([]),
      'PUT /appliance/api/v1/registries/bad': [403, { code: 'forbidden', message: 'Refused.' }],
    });
    render(<RegistriesPage />);
    await screen.findByText(/no registries/i);
    await userEvent.type(screen.getByLabelText('Registry host'), 'bad');
    await userEvent.type(screen.getByLabelText('User name'), 'u');
    await userEvent.click(screen.getByRole('button', { name: 'Add the registry' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Refused.');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Add the registry' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('deletes a registry after the confirmation', async () => {
    let items = [registry];
    const { sent } = stubApi({
      'GET /appliance/api/v1/registries': () => list(items),
      'DELETE /appliance/api/v1/registries/registry.example.test': () => {
        items = [];
        return [202];
      },
    });
    render(<RegistriesPage />);
    await userEvent.click(await screen.findByRole('button', { name: /Remove registry.example.test/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(sent['DELETE /appliance/api/v1/registries/registry.example.test']).toBeUndefined();
    await userEvent.click(screen.getByRole('button', { name: /Remove registry.example.test/ }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText(/no registries have credentials/i)).toBeInTheDocument();
    expect(sent['DELETE /appliance/api/v1/registries/registry.example.test']).toHaveLength(1);
  });

  it('closes the form of the registry it removes', async () => {
    let items = [registry];
    stubApi({
      'GET /appliance/api/v1/registries': () => list(items),
      'DELETE /appliance/api/v1/registries/registry.example.test': () => {
        items = [];
        return [202];
      },
    });
    render(<RegistriesPage />);
    await userEvent.click(await screen.findByRole('button', { name: /Edit registry.example.test/ }));
    expect(screen.getByRole('heading', { name: 'Edit registry.example.test' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Remove registry.example.test/ }));
    expect(screen.getByRole('dialog')).toHaveTextContent(/remove it from the components first/);
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText(/no registries have credentials/i)).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Add a registry' })).toBeInTheDocument();
  });

  it('adds, edits and removes a registry with a port', async () => {
    const withPort = { host: 'registry.example.test:5000', username: 'puller', passwordSet: true };
    const path = '/appliance/api/v1/registries/registry.example.test%3A5000';
    let items: object[] = [];
    const { sent } = stubApi({
      'GET /appliance/api/v1/registries': () => list(items),
      [`PUT ${path}`]: () => {
        items = [withPort];
        return [202];
      },
      [`DELETE ${path}`]: () => {
        items = [];
        return [202];
      },
    });
    render(<RegistriesPage />);
    await screen.findByText(/no registries/i);
    await userEvent.type(screen.getByLabelText('Registry host'), 'registry.example.test:5000');
    await userEvent.type(screen.getByLabelText('User name'), 'puller');
    await userEvent.type(screen.getByLabelText('Password'), 's3cret');
    await userEvent.click(screen.getByRole('button', { name: 'Add the registry' }));
    expect(await screen.findByText('registry.example.test:5000')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Edit registry.example.test:5000/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Save the credentials' }));
    await userEvent.click(await screen.findByRole('button', { name: /Remove registry.example.test:5000/ }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText(/no registries have credentials/i)).toBeInTheDocument();
    expect(sent[`PUT ${path}`]).toEqual([{ username: 'puller', password: 's3cret' }, { username: 'puller' }]);
    expect(sent[`DELETE ${path}`]).toHaveLength(1);
  });

  it('reports a registry that cannot be removed', async () => {
    stubApi({
      'GET /appliance/api/v1/registries': list([registry]),
      'DELETE /appliance/api/v1/registries/registry.example.test': [404, { code: 'not_found', message: 'No such registry.' }],
    });
    render(<RegistriesPage />);
    await userEvent.click(await screen.findByRole('button', { name: /Remove/ }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('No such registry.');
  });
});
