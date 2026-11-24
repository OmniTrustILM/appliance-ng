import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { hang, stubApi } from '../../setup/test-utils';
import { PlatformPage } from './PlatformPage';

const component = (name: string, over: object = {}) => ({
  name, title: name[0].toUpperCase() + name.slice(1), private: false, chosen: false, needsCredentials: false, needsMail: false, registered: true, ...over,
});
const managed = { database: { mode: 'managed' }, messaging: { mode: 'managed' }, identity: { mode: 'managed' } };
const platform = (components: object[], over: object = {}) =>
  [200, {
    ilmVersion: '2.17.0', hostName: 'ilm.example.test', components, dependencies: managed, utils: false, cbom: { enabled: false, storage: 'appliance' },
    setupComplete: true, ...over,
  }] as const;
const externalDatabase = {
  ...managed,
  database: { mode: 'external', host: 'db.example.test', port: 5432, name: 'ilm', username: 'ilm', passwordSet: true },
};
const TEST = 'POST /appliance/api/v1/platform/dependencies/test';

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

  it('shows the address to register a component that ILM does not register by itself', async () => {
    const url = 'http://email.ilm.svc.cluster.local:8080';
    stubApi({ 'GET /appliance/api/v1/platform': platform([component('email', { registered: false, url }), component('scanner')]) });
    open();
    const row = (await screen.findByRole('checkbox', { name: /Email/ })).closest('label');
    expect(row).toHaveTextContent(/not registered/i);
    expect(row).toHaveTextContent(url);
    expect(screen.getByRole('checkbox', { name: 'Scanner' }).closest('label')).not.toHaveTextContent(/not registered/i);
  });

  it('shows no address for an unregistered component that carries none', async () => {
    stubApi({ 'GET /appliance/api/v1/platform': platform([component('email', { registered: false })]) });
    open();
    const row = (await screen.findByRole('checkbox', { name: /Email/ })).closest('label');
    expect(row).toHaveTextContent(/not registered/i);
    expect(row?.querySelector('code')).toBeNull();
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

  it('explains why a dependency keeps its mode and saves the changed details of an external one with utils', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }),
      'PUT /appliance/api/v1/platform': [202],
    });
    open();
    const database = await screen.findByRole('group', { name: 'Database' });
    expect(within(database).getByText(/External PostgreSQL\. Switching between managed and external moves data/)).toBeInTheDocument();
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    expect(within(screen.getByRole('group', { name: 'Identity' })).getByText(/Managed Keycloak/)).toBeInTheDocument();
    expect(screen.getByLabelText('Database host')).toHaveValue('db.example.test');
    expect(screen.getByText(/One is set/)).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText('Database host'));
    await userEvent.type(screen.getByLabelText('Database host'), 'db2.example.test');
    await userEvent.type(screen.getByLabelText('Database password'), 'new secret');
    await userEvent.click(screen.getByRole('checkbox', { name: /Utils service/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/platform']).toEqual([{
      hostName: 'ilm.example.test', components: [], utils: true,
      dependencies: {
        database: { mode: 'external', host: 'db2.example.test', port: 5432, name: 'ilm', username: 'ilm', password: 'new secret' },
        messaging: { mode: 'managed' },
        identity: { mode: 'managed' },
      },
    }]);
  });

  it('keeps no password once it is saved, also when the page cannot reload', async () => {
    let loads = 0;
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': () => (loads++ === 0 ? platform([], { dependencies: externalDatabase }) : [500, { code: 'internal', message: 'x' }]),
      'PUT /appliance/api/v1/platform': [202],
    });
    open();
    await userEvent.type(await screen.findByLabelText('Database password'), 'new secret');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
    expect(screen.getByLabelText('Database password')).toHaveValue('');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(sent['PUT /appliance/api/v1/platform']).toHaveLength(2));
    expect(JSON.stringify(sent['PUT /appliance/api/v1/platform']?.[1])).not.toContain('new secret');
  });

  it('takes no edits while a save is pending', async () => {
    stubApi({ 'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }), 'PUT /appliance/api/v1/platform': hang });
    open();
    await userEvent.type(await screen.findByLabelText('Database password'), 'new secret');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.getByLabelText('Database password')).toBeDisabled());
    expect(screen.getByLabelText('ILM host name')).toBeDisabled();
  });

  it('shows a refused dependency field next to it', async () => {
    stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }),
      'PUT /appliance/api/v1/platform': [400, { code: 'invalid', field: 'dependencies.database.password', message: 'Enter the password.' }],
    });
    open();
    await userEvent.clear(await screen.findByLabelText('Database host'));
    await userEvent.type(screen.getByLabelText('Database host'), 'db2.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Enter the password.')).toBeInTheDocument();
    expect(screen.getByLabelText('Database password')).toHaveAttribute('aria-invalid', 'true');
  });

  it('tests the connection of an external dependency and shows the fixed reason', async () => {
    let answer: readonly [number, object] = [200, { ok: true }];
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }),
      [TEST]: () => answer,
    });
    open();
    await userEvent.click(await screen.findByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText('The connection works.')).toBeInTheDocument();
    expect(sent[TEST]).toEqual([{ kind: 'database', settings: { mode: 'external', host: 'db.example.test', port: 5432, name: 'ilm', username: 'ilm' } }]);

    answer = [200, { ok: false, reason: 'authFailed' }];
    await userEvent.click(screen.getByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText(/refused the user name or the password/)).toBeInTheDocument();

    answer = [400, { code: 'invalid', field: 'settings.password', message: 'Enter the password: the server changed.' }];
    await userEvent.click(screen.getByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText('Enter the password: the server changed.')).toBeInTheDocument();

    answer = [403, { code: 'forbidden', message: 'Refused.' }];
    await userEvent.click(screen.getByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText('Refused.')).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText('Database password'), 'x');
    expect(screen.queryByText('Refused.')).not.toBeInTheDocument();
  });

  it('reports a test the appliance did not answer', async () => {
    stubApi({ 'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }) });
    open();
    const fetch = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    await screen.findByRole('button', { name: 'Test the database connection' });
    fetch.mockRejectedValueOnce(new Error('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText(/cannot be reached/)).toBeInTheDocument();
  });

  it('sets the TLS of the database and the broker with their CAs and says what auth and Keycloak verify', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: {
        ...externalDatabase,
        database: { ...externalDatabase.database, tlsMode: 'verify-full', caSet: true },
        messaging: { mode: 'external', host: 'mq.example.test', port: 5672, virtualHost: '/', username: 'ilm', passwordSet: true, tls: false },
      } }),
      'PUT /appliance/api/v1/platform': [202],
    });
    open();
    expect(await screen.findByLabelText('Database TLS')).toHaveValue('verify-full');
    expect(screen.getByText(/auth service and Keycloak encrypt the connection when the server offers TLS but do not verify/)).toBeInTheDocument();
    expect(screen.getByText('A CA is set. Leave it blank to keep it.')).toBeInTheDocument();
    expect(screen.queryByLabelText('Broker CA certificate')).not.toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/Connect with TLS/));
    await userEvent.clear(screen.getByLabelText('Broker port'));
    await userEvent.type(screen.getByLabelText('Broker port'), '5671');
    await userEvent.type(screen.getByLabelText('Broker CA certificate'), 'BROKER CA');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
    const [body] = sent['PUT /appliance/api/v1/platform'] as { dependencies: Record<string, object> }[];
    expect(body.dependencies.database).toEqual({ mode: 'external', host: 'db.example.test', port: 5432, name: 'ilm', username: 'ilm', tlsMode: 'verify-full' });
    expect(body.dependencies.messaging).toEqual({ mode: 'external', host: 'mq.example.test', port: 5671, virtualHost: '/', username: 'ilm', tls: true, ca: 'BROKER CA' });
  });

  it('sends a new database CA only while the server is verified', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }),
      'PUT /appliance/api/v1/platform': [400, { code: 'invalid', field: 'dependencies.database.ca', message: 'Upload the CA.' }],
    });
    open();
    await userEvent.selectOptions(await screen.findByLabelText('Database TLS'), 'verify-full');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Upload the CA.')).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Database CA certificate'), 'DB CA');
    await userEvent.selectOptions(screen.getByLabelText('Database TLS'), 'require');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    const bodies = sent['PUT /appliance/api/v1/platform'] as { dependencies: { database: object } }[];
    expect(bodies[0].dependencies.database).toMatchObject({ tlsMode: 'verify-full' });
    expect(bodies[1].dependencies.database).toEqual({ mode: 'external', host: 'db.example.test', port: 5432, name: 'ilm', username: 'ilm', tlsMode: 'require' });
  });

  it('drops the result of a test whose settings were edited while it ran', async () => {
    let answer: (value: readonly [number, object]) => void = () => undefined;
    stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: externalDatabase }),
      [TEST]: () => new Promise((resolve) => { answer = resolve; }),
    });
    open();
    await userEvent.click(await screen.findByRole('button', { name: 'Test the database connection' }));
    await userEvent.type(screen.getByLabelText('Database host'), 'x');
    answer([200, { ok: true }]);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Test the database connection' })).toBeEnabled());
    expect(screen.queryByText('The connection works.')).not.toBeInTheDocument();
  });

  it('says a refused verify-full test may also be an untrusted certificate', async () => {
    stubApi({
      'GET /appliance/api/v1/platform': platform([], { dependencies: { ...externalDatabase, database: { ...externalDatabase.database, tlsMode: 'verify-full', caSet: true } } }),
      [TEST]: [200, { ok: false, reason: 'authFailed' }],
    });
    open();
    await userEvent.click(await screen.findByRole('button', { name: 'Test the database connection' }));
    expect(await screen.findByText(/or its certificate is not signed by the CA or does not name the host/)).toBeInTheDocument();
  });

  describe('CBOM repository', () => {
    const PUT = 'PUT /appliance/api/v1/platform';
    const external = {
      enabled: true, storage: 's3',
      s3: { endpoint: 'https://s3.example.test', bucket: 'cboms', region: '', accessKeySet: true, secretKeySet: true, caSet: true },
    };

    it('turns the repository on with the store on the appliance and says what turning it off keeps', async () => {
      const { sent } = stubApi({ 'GET /appliance/api/v1/platform': platform([]), [PUT]: [202] });
      open();
      const repository = await screen.findByRole('checkbox', { name: /CBOM repository/ });
      expect(repository).toHaveAccessibleName(/Only ILM reaches it/);
      expect(repository).toHaveAccessibleName(/Turned off, the CBOMs stored on the appliance are kept and still backed up/);
      expect(screen.queryByRole('radio', { name: /On the appliance/ })).toBeNull();
      await userEvent.click(repository);
      expect(screen.getByRole('radio', { name: /On the appliance/ })).toBeChecked();
      await userEvent.click(screen.getByRole('button', { name: 'Save' }));
      expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
      expect(sent[PUT]).toEqual([{ hostName: 'ilm.example.test', components: [], cbom: { enabled: true, storage: 'appliance' } }]);
    });

    it('sends an external bucket with only the secrets entered', async () => {
      const { sent } = stubApi({ 'GET /appliance/api/v1/platform': platform([], { cbom: { enabled: true, storage: 'appliance' } }), [PUT]: [202] });
      open();
      await userEvent.click(await screen.findByRole('radio', { name: /External S3/ }));
      await userEvent.type(screen.getByLabelText('S3 endpoint'), ' https://s3.example.test ');
      await userEvent.type(screen.getByLabelText('Bucket'), 'cboms');
      await userEvent.type(screen.getByLabelText('Access key'), 'AKIA');
      await userEvent.type(screen.getByLabelText('Secret key'), 'secret');
      await userEvent.type(screen.getByLabelText('CA bundle (optional)'), 'PEM');
      await userEvent.click(screen.getByRole('button', { name: 'Save' }));
      expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
      expect(sent[PUT]).toEqual([{
        hostName: 'ilm.example.test', components: [],
        cbom: { enabled: true, storage: 's3', s3: { endpoint: 'https://s3.example.test', bucket: 'cboms', region: '', accessKey: 'AKIA', secretKey: 'secret', ca: 'PEM' } },
      }]);
    });

    it('keeps the stored secrets, removes the stored CA and shows a refused field', async () => {
      const { sent } = stubApi({
        'GET /appliance/api/v1/platform': platform([], { cbom: external }),
        [PUT]: [400, { code: 'invalid', message: 'Enter the bucket.', field: 'cbom.s3.bucket' }],
      });
      open();
      expect(await screen.findByLabelText('S3 endpoint')).toHaveValue('https://s3.example.test');
      expect(screen.getByLabelText('Access key')).toHaveValue('');
      expect(screen.getAllByText('Leave blank to keep the stored value while the endpoint stays the same.')).toHaveLength(2);
      await userEvent.click(screen.getByRole('checkbox', { name: /Remove the stored CA bundle/ }));
      expect(screen.queryByLabelText('CA bundle (optional)')).toBeNull();
      await userEvent.clear(screen.getByLabelText('Bucket'));
      await userEvent.click(screen.getByRole('button', { name: 'Save' }));
      expect(await screen.findByText('Enter the bucket.')).toBeInTheDocument();
      expect(sent[PUT]).toEqual([{
        hostName: 'ilm.example.test', components: [],
        cbom: { enabled: true, storage: 's3', s3: { endpoint: 'https://s3.example.test', bucket: '', region: '', removeCa: true } },
      }]);
    });

    it('turns the repository off and keeps the chosen bucket', async () => {
      const { sent } = stubApi({ 'GET /appliance/api/v1/platform': platform([], { cbom: external }), [PUT]: [202] });
      open();
      await userEvent.click(await screen.findByRole('checkbox', { name: /CBOM repository/ }));
      expect(screen.queryByLabelText('S3 endpoint')).toBeNull();
      await userEvent.click(screen.getByRole('button', { name: 'Save' }));
      expect(await screen.findByText(/install in the background/i)).toBeInTheDocument();
      expect(sent[PUT]).toEqual([{
        hostName: 'ilm.example.test', components: [],
        cbom: { enabled: false, storage: 's3', s3: { endpoint: 'https://s3.example.test', bucket: 'cboms', region: '' } },
      }]);
    });
  });
});
