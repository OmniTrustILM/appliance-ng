import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../setup/test-utils';
import { WizardFlow } from './WizardFlow';

afterEach(() => vi.unstubAllGlobals());

const part2 = { setup: 'part2', conditions: [], versions: {}, rebootRequired: false };
const appliance = {
  cluster: { nodes: [{ hostName: 'node1.example.test', tags: [], maintenance: false }] },
  dns: [], ntp: [], tlsSource: 'builtin', trustSet: false, registries: [],
};
const catalogue = {
  items: [
    { name: 'ilm-core', title: 'ILM core', private: false },
    { name: 'scanner', title: 'Scanner', private: false },
    { name: 'hsm-bridge', title: 'HSM bridge', private: true },
  ],
};

function open(routes: Parameters<typeof stubApi>[0]) {
  const api = stubApi({
    'GET /appliance/api/v1/status': [200, part2],
    'GET /appliance/api/v1/appliance': [200, appliance],
    'GET /appliance/api/v1/catalogue': [200, catalogue],
    ...routes,
  });
  render(
    <MemoryRouter initialEntries={['/wizard/']}>
      <Routes>
        <Route path="/wizard/*" element={<WizardFlow />} />
        <Route path="/wizard/done" element={<p>done page</p>} />
        <Route path="/" element={<p>status page</p>} />
        <Route path="/restore" element={<p>restore page</p>} />
      </Routes>
    </MemoryRouter>,
  );
  return api;
}

const next = () => userEvent.click(screen.getByRole('button', { name: 'Continue' }));
const managed = { database: { mode: 'managed' }, messaging: { mode: 'managed' }, identity: { mode: 'managed' } };

describe('WizardFlow', () => {
  it('walks through the steps and submits the platform without mail', async () => {
    const { sent } = open({ 'PUT /appliance/api/v1/wizard/platform': [202] });
    expect(await screen.findByLabelText('ILM host name')).toHaveValue('node1.example.test');
    await userEvent.clear(screen.getByLabelText('ILM host name'));
    await userEvent.type(screen.getByLabelText('ILM host name'), 'ilm.example.test');
    await next();

    expect(screen.getByRole('heading', { name: 'Certificate' })).toBeInTheDocument();
    expect(screen.getByText(/upload.*later/i)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /root certificate/i })).not.toBeInTheDocument();
    expect(screen.getByText(/download it as soon as the installation starts/i)).toBeInTheDocument();
    await next();

    await userEvent.click(screen.getByLabelText('ILM core'));
    expect(screen.getByLabelText(/HSM bridge/)).toBeDisabled();
    expect(screen.getByText(/needs registry credentials, available later/)).toBeInTheDocument();
    await next();

    expect(screen.getByRole('heading', { name: 'Dependencies' })).toBeInTheDocument();
    await next();

    expect(screen.getByRole('heading', { name: 'Mail' })).toBeInTheDocument();
    await next();

    expect(screen.getByRole('heading', { name: 'Review' })).toBeInTheDocument();
    expect(screen.getByText('ilm.example.test')).toBeInTheDocument();
    expect(screen.getByText(/ILM core/)).toBeInTheDocument();
    expect(screen.getByText(/no mail/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));

    expect(await screen.findByRole('heading', { name: 'Installing the platform' })).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/wizard/platform']).toEqual([{ hostName: 'ilm.example.test', components: ['ilm-core'], dependencies: managed, utils: false }]);
  });

  it('sends the mail settings and keeps the password out of the summary', async () => {
    const { sent } = open({ 'PUT /appliance/api/v1/wizard/platform': [202] });
    await screen.findByLabelText('ILM host name');
    await next();
    await next();
    await next();
    await next();
    await userEvent.click(screen.getByLabelText('Send mail'));
    await userEvent.type(screen.getByLabelText('Mail server'), 'smtp.example.test');
    await userEvent.clear(screen.getByLabelText('Port'));
    await userEvent.type(screen.getByLabelText('Port'), '587');
    await userEvent.selectOptions(screen.getByLabelText('Security'), 'starttls');
    await userEvent.type(screen.getByLabelText('From address'), 'ilm@example.test');
    await userEvent.type(screen.getByLabelText('User name (optional)'), 'mailer');
    await userEvent.type(screen.getByLabelText('Password (optional)'), 'mail secret');
    await next();
    expect(screen.queryByText(/mail secret/)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    await screen.findByRole('heading', { name: 'Installing the platform' });
    expect(sent['PUT /appliance/api/v1/wizard/platform']).toEqual([{
      hostName: 'node1.example.test',
      components: [],
      dependencies: managed,
      utils: false,
      mail: { host: 'smtp.example.test', port: 587, security: 'starttls', from: 'ilm@example.test', username: 'mailer', password: 'mail secret' },
    }]);
  });

  it('prefills the answers, keeps a stored mail password on re-submit and starts at the progress', async () => {
    const { sent } = open({
      'GET /appliance/api/v1/appliance': [200, {
        ...appliance,
        mail: { host: 'smtp.example.test', port: 25, security: 'starttls', from: 'a@b.test', username: 'mailer', passwordSet: true },
        platform: { hostName: 'ilm.example.test', components: ['scanner'] },
      }],
      'GET /appliance/api/v1/platform': [200, { dependencies: managed, utils: false }],
      'PUT /appliance/api/v1/wizard/platform': [202],
    });
    expect(await screen.findByRole('heading', { name: 'Installing the platform' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Change the answers' }));
    expect(screen.getByLabelText('ILM host name')).toHaveValue('ilm.example.test');
    await next();
    await next();
    expect(screen.getByLabelText('Scanner')).toBeChecked();
    await next();
    await next();
    expect(screen.getByLabelText('Mail server')).toHaveValue('smtp.example.test');
    expect(screen.getByLabelText('Password (optional)')).toHaveValue('');
    expect(screen.getByText(/password is set/i)).toBeInTheDocument();
    await next();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    await screen.findByRole('heading', { name: 'Installing the platform' });
    expect(sent['PUT /appliance/api/v1/wizard/platform']).toEqual([{
      hostName: 'ilm.example.test',
      components: ['scanner'],
      dependencies: managed,
      utils: false,
      mail: { host: 'smtp.example.test', port: 25, security: 'starttls', from: 'a@b.test', username: 'mailer' },
    }]);
  });

  it('offers to keep the stored mail password only for the server and user name it was given for', async () => {
    open({
      'GET /appliance/api/v1/appliance': [200, {
        ...appliance,
        mail: { host: 'smtp.example.test', port: 25, security: 'starttls', from: 'a@b.test', username: 'mailer', passwordSet: true },
      }],
    });
    await screen.findByLabelText('ILM host name');
    await next();
    await next();
    await next();
    await next();
    expect(screen.getByText(/password is set/i)).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Mail server'), '2');
    expect(screen.queryByText(/password is set/i)).not.toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Mail server'), '{Backspace}');
    expect(screen.getByText(/password is set/i)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText('Send mail'));
    await userEvent.click(screen.getByLabelText('Send mail'));
    expect(screen.queryByText(/password is set/i)).not.toBeInTheDocument();
  });

  it.each([
    ['hostName', 'ILM host name', 'Name is not valid.'],
    ['components', 'Scanner', 'Pick public ones.'],
    ['mail.host', 'Mail server', 'Host is not valid.'],
    ['mail.password', 'Password (optional)', 'Password refused.'],
    ['mail.username', 'User name (optional)', 'Credentials need security starttls or tls; choose one, or remove the user name.'],
  ])('sends the admin back to the step of a refused field (%s)', async (field, label, message) => {
    open({
      'GET /appliance/api/v1/appliance': [200, {
        ...appliance,
        mail: { host: 'smtp.example.test', port: 25, security: 'none', from: 'a@b.test', passwordSet: false },
        platform: { hostName: 'ilm.example.test', components: ['scanner'] },
      }],
      'GET /appliance/api/v1/platform': [200, { dependencies: managed, utils: false }],
      'PUT /appliance/api/v1/wizard/platform': [400, { code: 'invalid', field, message }],
    });
    await screen.findByRole('heading', { name: 'Installing the platform' });
    await userEvent.click(screen.getByRole('button', { name: 'Change the answers' }));
    for (let i = 0; i < 5; i++) await next();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    expect(await screen.findByLabelText(label)).toBeInTheDocument();
    expect(screen.getByText(message)).toBeInTheDocument();
  });

  it('goes to the status page when the setup is complete, on load and on a refused submit', async () => {
    open({ 'GET /appliance/api/v1/status': [200, { ...part2, setup: 'complete' }] });
    expect(await screen.findByText('status page')).toBeInTheDocument();
  });

  it('leaves for the status page when the submit finds the setup complete', async () => {
    open({ 'PUT /appliance/api/v1/wizard/platform': [409, { code: 'setup_complete', message: 'x' }] });
    await screen.findByLabelText('ILM host name');
    for (let i = 0; i < 5; i++) await next();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    expect(await screen.findByText('status page')).toBeInTheDocument();
  });

  it('goes to the restore page when a restore is pending', async () => {
    open({ 'GET /appliance/api/v1/status': [200, { ...part2, setup: 'restore' }] });
    expect(await screen.findByText('restore page')).toBeInTheDocument();
    expect(screen.queryByLabelText('ILM host name')).not.toBeInTheDocument();
  });

  it('shows the message of a refused submit', async () => {
    const message = 'The appliance is waiting for a restore.';
    open({ 'PUT /appliance/api/v1/wizard/platform': [409, { code: 'restore_pending', message }] });
    await screen.findByLabelText('ILM host name');
    for (let i = 0; i < 5; i++) await next();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(message);
    expect(screen.queryByText(/not at this step/)).not.toBeInTheDocument();
  });

  it('shows a general message for other answers and when the appliance cannot be reached', async () => {
    open({ 'PUT /appliance/api/v1/wizard/platform': [409] });
    await screen.findByLabelText('ILM host name');
    for (let i = 0; i < 5; i++) await next();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('not at this step');
    // The previous steps can be revisited.
    await userEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(screen.getByRole('heading', { name: 'Mail' })).toBeInTheDocument();
  });

  it('reports a failed load', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [500, { code: 'internal', message: 'x' } ] });
    render(<MemoryRouter><WizardFlow /></MemoryRouter>);
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });

  it('reports a failed load when the chosen dependencies cannot be read, instead of sending managed ones', async () => {
    open({
      'GET /appliance/api/v1/appliance': [200, { ...appliance, platform: { hostName: 'ilm.example.test', components: [] } }],
      'GET /appliance/api/v1/platform': [500, { code: 'internal', message: 'x' }],
    });
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    expect(screen.queryByRole('button', { name: 'Change the answers' })).not.toBeInTheDocument();
  });

  it('checks the mail fields before it goes on', async () => {
    open({});
    await screen.findByLabelText('ILM host name');
    await next();
    await next();
    await next();
    await next();
    await userEvent.click(screen.getByLabelText('Send mail'));
    await next();
    expect(screen.getByLabelText('Mail server')).toHaveAccessibleDescription(/required/i);
    expect(screen.getByLabelText('From address')).toHaveAccessibleDescription(/required/i);
    await userEvent.type(screen.getByLabelText('Mail server'), 'smtp.example.test');
    await userEvent.type(screen.getByLabelText('From address'), 'a@b.test');
    await userEvent.clear(screen.getByLabelText('Port'));
    await userEvent.type(screen.getByLabelText('Port'), '99999');
    await next();
    expect(screen.getByLabelText('Port')).toHaveAccessibleDescription(/1 and 65535/);
    await userEvent.clear(screen.getByLabelText('Port'));
    await userEvent.type(screen.getByLabelText('Port'), '25');
    await userEvent.type(screen.getByLabelText('User name (optional)'), 'mailer');
    await next();
    expect(screen.getByLabelText('User name (optional)')).toHaveAccessibleDescription(/starttls or tls/);
  });

  it('asks for a host name', async () => {
    open({});
    await userEvent.clear(await screen.findByLabelText('ILM host name'));
    await next();
    expect(screen.getByLabelText('ILM host name')).toHaveAccessibleDescription(/required/i);
  });

  it('chooses external dependencies and utils, tests one and sends them', async () => {
    const { sent } = open({
      'PUT /appliance/api/v1/wizard/platform': [202],
      'POST /appliance/api/v1/platform/dependencies/test': [200, { ok: false, reason: 'unreachable' }],
    });
    await screen.findByLabelText('ILM host name');
    await next();
    await next();
    await next();
    expect(screen.getByLabelText(/Managed PostgreSQL/)).toBeChecked();
    await userEvent.click(screen.getByLabelText(/External PostgreSQL/));
    await userEvent.type(screen.getByLabelText('Database host'), 'db.example.test');
    await userEvent.type(screen.getByLabelText('Database name'), 'ilm');
    await userEvent.type(screen.getByLabelText('Database user name'), 'ilm');
    await userEvent.type(screen.getByLabelText('Database password'), 'db secret');
    await userEvent.selectOptions(screen.getByLabelText('Database TLS'), 'verify-full');
    await userEvent.type(screen.getByLabelText('Database CA certificate'), 'DB CA');
    await userEvent.click(screen.getByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText(/cannot be reached from the appliance/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/External RabbitMQ/));
    await userEvent.type(screen.getByLabelText('Broker host'), 'mq.example.test');
    await userEvent.type(screen.getByLabelText('Virtual host'), 'ilm');
    await userEvent.type(screen.getByLabelText('Broker user name'), 'ilm');
    await userEvent.type(screen.getByLabelText('Broker password'), 'mq secret');
    await userEvent.click(screen.getByLabelText(/External OIDC provider/));
    await userEvent.type(screen.getByLabelText('Issuer URL'), 'https://idp.example.test/realms/ilm');
    await userEvent.type(screen.getByLabelText('Client ID'), 'ilm');
    await userEvent.type(screen.getByLabelText('Scopes'), 'openid profile');
    await userEvent.type(screen.getByLabelText('Accepted audiences'), 'ilm');
    await userEvent.type(screen.getByLabelText('Client secret'), 'idp secret');
    await userEvent.click(screen.getByLabelText(/Utils service/));
    await next();
    await next();
    expect(screen.getByText(/database db\.example\.test:5432\/ilm, broker mq\.example\.test:5672, identity provider https/)).toBeInTheDocument();
    expect(screen.getByText('Installed')).toBeInTheDocument();
    expect(screen.queryByText(/secret/)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    await screen.findByRole('heading', { name: 'Installing the platform' });
    expect(sent['PUT /appliance/api/v1/wizard/platform']).toEqual([{
      hostName: 'node1.example.test', components: [], utils: true,
      dependencies: {
        database: { mode: 'external', host: 'db.example.test', port: 5432, name: 'ilm', username: 'ilm', password: 'db secret', tlsMode: 'verify-full', ca: 'DB CA' },
        messaging: { mode: 'external', host: 'mq.example.test', port: 5672, virtualHost: 'ilm', username: 'ilm', password: 'mq secret' },
        identity: { mode: 'external', issuer: 'https://idp.example.test/realms/ilm', clientId: 'ilm', scopes: ['openid', 'profile'], audiences: ['ilm'], clientSecret: 'idp secret' },
      },
    }]);
    await userEvent.click(screen.getByRole('button', { name: 'Change the answers' }));
    for (let i = 0; i < 3; i++) await next();
    expect(screen.getByLabelText('Database password')).toHaveValue('');
    expect(screen.getAllByText(/One is set/)).toHaveLength(3);
    expect(screen.getByLabelText('Database CA certificate')).toHaveValue('');
    expect(screen.getByText('A CA is set. Leave it blank to keep it.')).toBeInTheDocument();
  });

  it('prefills the chosen dependencies and sends the admin back to a refused one', async () => {
    open({
      'GET /appliance/api/v1/appliance': [200, { ...appliance, platform: { hostName: 'ilm.example.test', components: [] } }],
      'GET /appliance/api/v1/platform': [200, {
        ilmVersion: '2.20.0', hostName: 'ilm.example.test', components: [], utils: true, setupComplete: false,
        dependencies: { ...managed, database: { mode: 'external', host: 'db.example.test', port: 5433, name: 'ilm', username: 'ilm', passwordSet: true } },
      }],
      'PUT /appliance/api/v1/wizard/platform': [400, { code: 'invalid', field: 'dependencies.database.host', message: 'Host refused.' }],
    });
    await screen.findByRole('heading', { name: 'Installing the platform' });
    await userEvent.click(screen.getByRole('button', { name: 'Change the answers' }));
    for (let i = 0; i < 5; i++) await next();
    await userEvent.click(screen.getByRole('button', { name: 'Install' }));
    expect(await screen.findByLabelText('Database host')).toHaveValue('db.example.test');
    expect(screen.getByLabelText('Database port')).toHaveValue('5433');
    expect(screen.getByText('Host refused.')).toBeInTheDocument();
    expect(screen.getByLabelText(/Utils service/)).toBeChecked();
  });
});
