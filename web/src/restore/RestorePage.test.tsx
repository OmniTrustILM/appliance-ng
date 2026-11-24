import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../setup/test-utils';
import { RestorePage, progressRefreshMs } from './RestorePage';

const STATE = 'GET /appliance/api/v1/restore';
const UPLOAD = 'PUT /appliance/api/v1/restore/upload';
const LIST = 'POST /appliance/api/v1/restore/list';
const FETCH = 'POST /appliance/api/v1/restore/fetch';
const INSPECT = 'POST /appliance/api/v1/restore/inspect';
const START = 'POST /appliance/api/v1/restore/start';
const RESUME = 'POST /appliance/api/v1/restore/resume';

const backupName = 'old-node-20261101T020000Z-1.0.0.ilmbackup';
const passphrase = 'the kit passphrase';
const summary = {
  inspection: 'check-1', backup: backupName, inPlace: false, time: '2026-11-01T02:00:00Z', hostName: 'old-node', ilmHostName: 'ilm.old.example.test',
  nodeHostName: 'new-node', ilm: '2.20.0', operator: '1.1.0', appliance: '1.0.0', database: 'managed', databaseIncluded: true,
  accounts: ['admin', 'alice'], clashes: ['admin'], components: ['core'],
  dependencies: {
    database: { mode: 'external', host: 'db.old.example.test', port: 5432, name: 'ilm' },
    messaging: { mode: 'managed' },
    identity: { mode: 'managed' },
  },
};
const waiting = { setup: 'restore' };

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function renderPage() {
  return render(<MemoryRouter><RestorePage /></MemoryRouter>);
}

async function giveKit(button = 'Check the backup') {
  await userEvent.upload(await screen.findByLabelText(/recovery kit/i), new File(['kit'], 'kit.tar.age'));
  await userEvent.type(screen.getByLabelText('Passphrase'), passphrase);
  await userEvent.click(screen.getByRole('button', { name: button }));
}

async function uploadAndCheck() {
  await userEvent.upload(await screen.findByLabelText(/^backup file/i), new File(['encrypted backup'], backupName));
  await userEvent.click(screen.getByRole('button', { name: 'Upload the backup' }));
  await giveKit();
}

describe('RestorePage', () => {
  it('uploads a backup, checks it with the kit and shows both host names, the ILM host name and the clashes', async () => {
    const { sent } = stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary] });
    renderPage();
    await uploadAndCheck();
    expect(sent[UPLOAD]).toEqual(['encrypted backup']);
    const [form] = sent[INSPECT] as Record<string, unknown>[];
    expect(form).toMatchObject({ source: 'upload', name: backupName, passphrase });
    expect(form.kit).toBeInstanceOf(File);

    expect(await screen.findByText('old-node')).toBeInTheDocument();
    expect(screen.getByText(/named in the backup's manifest, which does not prove where the backup comes from/i)).toBeInTheDocument();
    expect(screen.getByText('new-node')).toBeInTheDocument();
    expect(screen.getByText('Operator version in the backup').nextElementSibling).toHaveTextContent('1.1.0');
    expect(screen.getByText('Appliance version in the backup').nextElementSibling).toHaveTextContent('1.0.0');
    expect(screen.getByLabelText('ILM host name')).toHaveValue('ilm.old.example.test');
    expect(screen.getByText(/the backup's account wins/i)).toHaveTextContent('admin');
    expect(screen.queryByDisplayValue(passphrase)).not.toBeInTheDocument();
  });

  it('starts the restore with the renamed ILM host name and follows its progress', async () => {
    let state: object = waiting;
    const { sent } = stubApi({
      [STATE]: () => [200, state],
      [UPLOAD]: [204],
      [INSPECT]: [200, summary],
      [START]: () => {
        state = { setup: 'restore', restore: { backup: backupName, inPlace: false, step: 'database' } };
        return [202];
      },
    });
    renderPage();
    await uploadAndCheck();
    const host = await screen.findByLabelText('ILM host name');
    await userEvent.clear(host);
    await userEvent.type(host, 'ilm.new.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Restore the backup' }));
    expect(sent[START]).toEqual([{ inspection: 'check-1', inPlace: false, ilmHostName: 'ilm.new.example.test', confirm: true }]);
    expect(await screen.findByRole('heading', { name: `Restoring ${backupName}` })).toBeInTheDocument();
    expect(screen.getByText('Load the database').closest('li')).toHaveAttribute('aria-current', 'step');
  });

  it('keeps the backup\'s dependencies or starts the restore with dependencies chosen anew', async () => {
    let answer: readonly [number, object?] = [400, { code: 'invalid', field: 'dependencies.database.password', message: 'Enter the password.' }];
    const { sent } = stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary], [START]: () => answer });
    renderPage();
    await uploadAndCheck();
    expect(await screen.findByText(/database db\.old\.example\.test:5432\/ilm, managed broker, managed Keycloak/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/Choose the dependencies anew/));
    expect(screen.getByLabelText('Database host')).toHaveValue('db.old.example.test');
    await userEvent.type(screen.getByLabelText('Database user name'), 'ilm');
    await userEvent.click(screen.getByLabelText(/External OIDC provider/));
    await userEvent.type(screen.getByLabelText('Issuer URL'), 'https://idp.example.test');
    await userEvent.type(screen.getByLabelText('Client ID'), 'ilm');
    await userEvent.type(screen.getByLabelText('Client secret'), 'idp secret');
    await userEvent.click(screen.getByRole('button', { name: 'Restore the backup' }));
    expect(await screen.findByText('Enter the password.')).toBeInTheDocument();
    expect(screen.getByLabelText('Database password')).toHaveAttribute('aria-invalid', 'true');

    answer = [202];
    await userEvent.type(screen.getByLabelText('Database password'), 'db secret');
    await userEvent.click(screen.getByRole('button', { name: 'Restore the backup' }));
    await waitFor(() => expect(sent[START]).toHaveLength(2));
    expect(sent[START]?.[1]).toEqual({
      inspection: 'check-1', inPlace: false, ilmHostName: 'ilm.old.example.test', confirm: true,
      dependencies: {
        database: { mode: 'external', host: 'db.old.example.test', port: 5432, name: 'ilm', username: 'ilm', password: 'db secret' },
        messaging: { mode: 'managed' },
        identity: { mode: 'external', issuer: 'https://idp.example.test', clientId: 'ilm', scopes: [], audiences: [], clientSecret: 'idp secret' },
      },
    });
  });

  it('warns that the dump replaces the content of the external database it loads into', async () => {
    stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary] });
    renderPage();
    await uploadAndCheck();
    expect(await screen.findByText(/replaces the content of the external database db\.old\.example\.test:5432\/ilm/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/Choose the dependencies anew/));
    await userEvent.clear(screen.getByLabelText('Database host'));
    await userEvent.type(screen.getByLabelText('Database host'), 'db.test.example.test');
    expect(screen.getByText(/replaces the content of the external database db\.test\.example\.test:5432\/ilm/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/Managed PostgreSQL/));
    expect(screen.queryByText(/replaces the content of the external database/)).not.toBeInTheDocument();
  });

  it('gives no database warning when the backup holds no dump', async () => {
    stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, { ...summary, database: 'external', databaseIncluded: false }] });
    renderPage();
    await uploadAndCheck();
    expect(await screen.findByText('old-node')).toBeInTheDocument();
    expect(screen.queryByText(/replaces the content of the external database/)).not.toBeInTheDocument();
  });

  it('shows a refusal of a dependency that was not chosen anew', async () => {
    stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary],
      [START]: [400, { code: 'invalid', field: 'dependencies.database.password', message: 'The backup holds no password.' }] });
    renderPage();
    await uploadAndCheck();
    await userEvent.click(await screen.findByRole('button', { name: 'Restore the backup' }));
    expect(await screen.findByText('The backup holds no password.')).toBeInTheDocument();
  });

  it('keeps the backup\'s dependencies when they are not chosen anew', async () => {
    const { sent } = stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary], [START]: [202] });
    renderPage();
    await uploadAndCheck();
    await userEvent.click(await screen.findByLabelText(/Choose the dependencies anew/));
    await userEvent.click(screen.getByLabelText(/Choose the dependencies anew/));
    expect(screen.queryByLabelText('Database host')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Restore the backup' }));
    await waitFor(() => expect(sent[START]).toEqual([{ inspection: 'check-1', inPlace: false, ilmHostName: 'ilm.old.example.test', confirm: true }]));
  });

  it('fetches a backup from a target entered on the page', async () => {
    const files = { files: [
      { name: backupName, sizeBytes: 2048, modified: '2026-11-01T02:01:00Z' },
      { name: 'old-node-20261031T020000Z-1.0.0.ilmbackup', sizeBytes: 1024, modified: '2026-10-31T02:01:00Z' },
    ] };
    const { sent } = stubApi({ [STATE]: [200, waiting], [LIST]: [200, files], [FETCH]: [204], [INSPECT]: [200, summary] });
    renderPage();
    await userEvent.click(await screen.findByLabelText('Fetch it from a target'));
    await userEvent.type(screen.getByLabelText('Endpoint'), 'https://s3.example.test');
    await userEvent.type(screen.getByLabelText('Bucket'), 'ilm');
    await userEvent.type(screen.getByLabelText('Access key'), 'access');
    await userEvent.type(screen.getByLabelText('Secret key'), 'secret');
    await userEvent.click(screen.getByRole('button', { name: 'List the backups' }));
    await userEvent.click(await screen.findByLabelText(new RegExp(backupName)));
    await userEvent.click(screen.getByRole('button', { name: 'Fetch the backup' }));
    expect(sent[LIST]).toEqual([{ target: expect.objectContaining({ type: 's3', s3: expect.objectContaining({ bucket: 'ilm', secretKey: 'secret' }) }) }]);
    expect(sent[FETCH]).toEqual([{ name: backupName, target: expect.objectContaining({ type: 's3' }) }]);
    await giveKit();
    expect(await screen.findByText('old-node')).toBeInTheDocument();
  });

  it('drops a listing that arrives after the target was changed', async () => {
    let answer: (reply: readonly [number, unknown]) => void = () => undefined;
    stubApi({ [STATE]: [200, waiting], [LIST]: () => new Promise((resolve) => { answer = resolve; }) });
    renderPage();
    await userEvent.click(await screen.findByLabelText('Fetch it from a target'));
    await userEvent.click(screen.getByRole('button', { name: 'List the backups' }));
    await userEvent.type(screen.getByLabelText('Bucket'), 'other');
    answer([200, { files: [{ name: backupName, sizeBytes: 2048, modified: '2026-11-01T02:01:00Z' }] }]);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByLabelText(new RegExp(backupName))).not.toBeInTheDocument();
  });

  it('keeps the other backup choice disabled while the backup is checked', async () => {
    let answer: (reply: readonly [number, unknown]) => void = () => undefined;
    stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: () => new Promise((resolve) => { answer = resolve; }) });
    renderPage();
    await uploadAndCheck();
    expect(screen.getByRole('button', { name: 'Choose another backup' })).toBeDisabled();
    answer([200, summary]);
    expect(await screen.findByText('old-node')).toBeInTheDocument();
  });

  it('drops an upload that finishes after the target source was chosen', async () => {
    let answer: (reply: readonly [number, unknown]) => void = () => undefined;
    stubApi({ [STATE]: [200, waiting], [UPLOAD]: () => new Promise((resolve) => { answer = resolve; }) });
    renderPage();
    await userEvent.upload(await screen.findByLabelText(/^backup file/i), new File(['encrypted backup'], backupName));
    await userEvent.click(screen.getByRole('button', { name: 'Upload the backup' }));
    await userEvent.click(screen.getByLabelText('Fetch it from a target'));
    answer([204, undefined]);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByLabelText(/recovery kit/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Bucket')).toBeInTheDocument();
  });

  it('keeps the other backup choice disabled while the restore is started', async () => {
    let answer: (reply: readonly [number, unknown]) => void = () => undefined;
    stubApi({ [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary], [START]: () => new Promise((resolve) => { answer = resolve; }) });
    renderPage();
    await uploadAndCheck();
    await userEvent.click(await screen.findByRole('button', { name: 'Restore the backup' }));
    expect(screen.getByRole('button', { name: 'Choose another backup' })).toBeDisabled();
    answer([400, { code: 'invalid', field: 'confirm', message: 'Confirm that the backup replaces what the appliance holds now.' }]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Choose another backup' })).toBeEnabled();
  });

  it('shows why a target could not be listed', async () => {
    stubApi({ [STATE]: [200, waiting], [LIST]: [502, { code: 'authFailed', message: 'The target refused the credentials.' }] });
    renderPage();
    await userEvent.click(await screen.findByLabelText('Fetch it from a target'));
    await userEvent.click(screen.getByRole('button', { name: 'List the backups' }));
    expect(await screen.findByText('The target refused the credentials.')).toBeInTheDocument();
  });

  it('shows a refusal about a field the target form does not show, and forgets a listing once the target changes', async () => {
    let listed = 0;
    stubApi({
      [STATE]: [200, waiting],
      [LIST]: () => (++listed === 1
        ? [400, { code: 'invalid', field: 'timeZone', message: 'Choose a known time zone.' }]
        : [200, { files: [{ name: backupName, sizeBytes: 2048, modified: '2026-11-01T02:01:00Z' }] }]),
    });
    renderPage();
    await userEvent.click(await screen.findByLabelText('Fetch it from a target'));
    await userEvent.click(screen.getByRole('button', { name: 'List the backups' }));
    expect(await screen.findByText('Choose a known time zone.')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'List the backups' }));
    expect(await screen.findByLabelText(new RegExp(backupName))).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Bucket'), 'other');
    expect(screen.queryByLabelText(new RegExp(backupName))).not.toBeInTheDocument();
  });

  it('shows a refused start that does not concern the ILM host name apart from it', async () => {
    stubApi({
      [STATE]: [200, waiting], [UPLOAD]: [204], [INSPECT]: [200, summary],
      [START]: [400, { code: 'invalid', field: 'confirm', message: 'Confirm that the backup replaces what the appliance holds now.' }],
    });
    renderPage();
    await uploadAndCheck();
    await userEvent.click(await screen.findByRole('button', { name: 'Restore the backup' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/confirm that the backup replaces/i);
    expect(screen.getByLabelText('ILM host name')).not.toHaveAccessibleDescription(/confirm/i);
  });

  it('shows a refused passphrase at its field and clears it', async () => {
    stubApi({
      [STATE]: [200, waiting], [UPLOAD]: [204],
      [INSPECT]: [400, { code: 'wrong_passphrase', message: 'The passphrase does not open the kit.', field: 'passphrase' }],
    });
    renderPage();
    await uploadAndCheck();
    expect(await screen.findByText('The passphrase does not open the kit.')).toBeInTheDocument();
    expect(screen.getByLabelText('Passphrase')).toHaveValue('');
  });

  it('shows a refused backup with the reason', async () => {
    stubApi({
      [STATE]: [200, waiting], [UPLOAD]: [204],
      [INSPECT]: [409, { code: 'version_unavailable', message: 'The backup holds ILM 2.19.0, but this appliance has ILM 2.20.0.' }],
    });
    renderPage();
    await uploadAndCheck();
    expect(await screen.findByText(/holds ILM 2\.19\.0/)).toBeInTheDocument();
  });

  it('asks for the kit and passphrase again while the restore waits for them', async () => {
    let state: object = { setup: 'restore', restore: { backup: backupName, inPlace: false, step: 'waitingForKit', pausedStep: 'stop' } };
    const { sent } = stubApi({
      [STATE]: () => [200, state],
      [RESUME]: () => {
        state = { setup: 'restore', restore: { backup: backupName, inPlace: false, step: 'stop' } };
        return [202];
      },
    });
    renderPage();
    expect(await screen.findByText(/the manager restarted/i)).toBeInTheDocument();
    await giveKit('Resume the restore');
    const [form] = sent[RESUME] as Record<string, unknown>[];
    expect(form.passphrase).toBe(passphrase);
    expect(form.kit).toBeInstanceOf(File);
    expect(await screen.findByText('Stop ILM')).toBeInTheDocument();
    expect(screen.getByText('Stop ILM').closest('li')).toHaveAttribute('aria-current', 'step');
  });

  it('reads the progress again while the restore runs', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout'] });
    let reads = 0;
    stubApi({
      [STATE]: () => [200, { setup: 'restore', restore: { backup: backupName, inPlace: false, step: ++reads < 2 ? 'secrets' : 'appliance' } }],
    });
    renderPage();
    expect(await screen.findByText('Write the appliance secrets')).toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(progressRefreshMs);
    await vi.waitFor(() => expect(screen.getByText('Restore the appliance settings').closest('li')).toHaveAttribute('aria-current', 'step'));
  });

  it('shows a failed restore with its reason and lets a fresh appliance choose another backup', async () => {
    stubApi({ [STATE]: [200, { setup: 'restore', restore: { backup: backupName, inPlace: false, step: 'database', message: 'timeout' } }] });
    renderPage();
    expect(await screen.findByText(/stopped at "Load the database": a step did not finish in time\. ILM was started again\./i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Resume the restore' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Choose another backup' }));
    expect(screen.getByLabelText('Upload a backup file')).toBeChecked();
  });

  it('does not say that ILM was started again for a failure before ILM was stopped', async () => {
    stubApi({ [STATE]: [200, { setup: 'complete', restore: { backup: backupName, inPlace: true, step: 'appliance', message: 'failed' } }] });
    renderPage();
    expect(await screen.findByText(/stopped at "Restore the appliance settings": a step failed/i)).toBeInTheDocument();
    expect(screen.queryByText(/ILM was started again/i)).not.toBeInTheDocument();
  });

  it('reports a finished restore in place with its warning', async () => {
    stubApi({ [STATE]: [200, { setup: 'complete', restore: {
      backup: backupName, inPlace: true, step: 'done', warning: 'the uploaded certificate does not cover ilm.example.test',
    } }] });
    renderPage();
    expect(await screen.findByText(/the restore finished/i)).toBeInTheDocument();
    expect(screen.getByText(/does not cover ilm\.example\.test/)).toBeInTheDocument();
    expect(screen.queryByText('Bring the platform up')).not.toBeInTheDocument();
  });

  it('asks for a new recovery kit once a fresh restore is done', async () => {
    stubApi({ [STATE]: [200, { setup: 'complete', restore: { backup: backupName, inPlace: false, step: 'done' } }] });
    renderPage();
    expect(await screen.findByText(/the restore finished/i)).toBeInTheDocument();
    expect(screen.getByText(/backups wait until a new recovery kit is issued and confirmed/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Issue a new recovery kit' })).toHaveAttribute('href', '/recovery-kit');
  });

  it('asks for no new recovery kit after a restore in place', async () => {
    stubApi({ [STATE]: [200, { setup: 'complete', restore: { backup: backupName, inPlace: true, step: 'done' } }] });
    renderPage();
    expect(await screen.findByText(/the restore finished/i)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Issue a new recovery kit' })).not.toBeInTheDocument();
  });

  it('says that no restore waits on a set-up appliance', async () => {
    stubApi({ [STATE]: [200, { setup: 'complete' }] });
    renderPage();
    const note = await screen.findByText(/no restore is in progress/i);
    expect(within(note).getByRole('link', { name: 'Backups' })).toHaveAttribute('href', '/backups');
  });
});
