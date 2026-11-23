import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { requestTimeoutMs, slowRequestTimeoutMs } from '../../api/client';
import { hang, stubApi } from '../../setup/test-utils';
import { TargetForm, type Target } from './TargetForm';
import { enterAgainHint } from './TargetFields';
import { browserTimeZone } from './targetValues';

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const fingerprint = 'SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG';
const HOSTKEY = 'POST /appliance/api/v1/backups/targets/hostkey';
const noSecrets = { accessKey: false, secretKey: false, ca: false, password: false, privateKey: false };

function renderForm(editing?: Target) {
  const onSaved = vi.fn();
  const onCancel = vi.fn();
  render(<TargetForm editing={editing} onSaved={onSaved} onCancel={onCancel} />);
  return { onSaved, onCancel };
}

describe('TargetForm', () => {
  it('adds an S3 target with the default schedule, the browser time zone and 14 kept backups', async () => {
    const { sent } = stubApi({ 'PUT /appliance/api/v1/backups/targets/offsite': [202] });
    const { onSaved } = renderForm();
    expect(screen.getByLabelText('Schedule')).toHaveValue('0 2 * * *');
    expect(screen.getByLabelText('Time zone')).toHaveValue(browserTimeZone());
    expect(screen.getByLabelText('Backups kept')).toHaveValue('14');
    await userEvent.type(screen.getByLabelText('Name'), 'offsite');
    await userEvent.type(screen.getByLabelText('Endpoint'), 'https://s3.example.test');
    await userEvent.type(screen.getByLabelText('Bucket'), 'ilm');
    await userEvent.type(screen.getByLabelText('Access key'), 'AK');
    await userEvent.type(screen.getByLabelText('Secret key'), 'SK');
    await userEvent.type(screen.getByLabelText('CA bundle (optional)'), 'PEM');
    await userEvent.clear(screen.getByLabelText('Time zone'));
    await userEvent.type(screen.getByLabelText('Time zone'), 'Europe/Prague');
    await userEvent.click(screen.getByRole('button', { name: 'Add the target' }));
    expect(onSaved).toHaveBeenCalled();
    expect(sent['PUT /appliance/api/v1/backups/targets/offsite']).toEqual([{
      type: 's3', schedule: '0 2 * * *', timeZone: 'Europe/Prague', retention: 14,
      s3: { endpoint: 'https://s3.example.test', bucket: 'ilm', accessKey: 'AK', secretKey: 'SK', ca: 'PEM' },
    }]);
  });

  it('shows the server\'s field error at its field and other errors above the form', async () => {
    let answer: readonly [number, unknown?] = [400, { code: 'invalid', message: 'Enter the endpoint as https://host.', field: 's3.endpoint' }];
    stubApi({ 'PUT /appliance/api/v1/backups/targets/offsite': () => answer });
    renderForm();
    await userEvent.type(screen.getByLabelText('Name'), 'offsite');
    await userEvent.click(screen.getByRole('button', { name: 'Add the target' }));
    expect(await screen.findByText('Enter the endpoint as https://host.')).toBeInTheDocument();
    expect(screen.getByLabelText('Endpoint')).toHaveAttribute('aria-invalid', 'true');
    answer = [409, { code: 'too_many_targets', message: 'At most 16 targets.' }];
    await userEvent.click(screen.getByRole('button', { name: 'Add the target' }));
    expect(await screen.findByText('At most 16 targets.')).toBeInTheDocument();
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('offline')));
    await userEvent.click(screen.getByRole('button', { name: 'Add the target' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('adds an SFTP target only once the scanned host key is accepted', async () => {
    const { sent } = stubApi({
      [HOSTKEY]: [200, { fingerprint }],
      'PUT /appliance/api/v1/backups/targets/sftp': [202],
    });
    renderForm();
    await userEvent.click(screen.getByLabelText(/^SFTP/));
    await userEvent.type(screen.getByLabelText('Name'), 'sftp');
    await userEvent.type(screen.getByLabelText('Host'), 'sftp.example.test');
    await userEvent.clear(screen.getByLabelText('Port'));
    await userEvent.type(screen.getByLabelText('Port'), '2222');
    await userEvent.type(screen.getByLabelText('Directory'), '/backups');
    await userEvent.type(screen.getByLabelText('User name'), 'ilm');
    await userEvent.type(screen.getByLabelText('Password'), 'pw');
    const save = screen.getByRole('button', { name: 'Add the target' });
    expect(save).toBeDisabled();

    await userEvent.click(screen.getByRole('button', { name: 'Read the host key' }));
    expect(await screen.findByText(fingerprint)).toBeInTheDocument();
    expect(sent[HOSTKEY]).toEqual([{ host: 'sftp.example.test', port: 2222 }]);
    expect(save).toBeDisabled();
    await userEvent.click(screen.getByRole('button', { name: 'Accept this host key' }));
    expect(screen.getByText(/^Accepted:/)).toBeInTheDocument();
    expect(save).toBeEnabled();

    // Another server needs its own key.
    await userEvent.type(screen.getByLabelText('Host'), 'x');
    expect(save).toBeDisabled();
    await userEvent.type(screen.getByLabelText('Host'), '{Backspace}');
    await userEvent.click(screen.getByRole('button', { name: 'Read the host key' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Accept this host key' }));
    await userEvent.click(save);
    expect(sent['PUT /appliance/api/v1/backups/targets/sftp']).toEqual([{
      type: 'sftp', schedule: '0 2 * * *', timeZone: browserTimeZone(), retention: 14,
      sftp: { host: 'sftp.example.test', port: 2222, path: '/backups', user: 'ilm', hostKey: fingerprint, password: 'pw' },
    }]);
  });

  it('offers a scanned host key only for the server it was read from', async () => {
    stubApi({ [HOSTKEY]: [200, { fingerprint }] });
    renderForm();
    await userEvent.click(screen.getByLabelText(/^SFTP/));
    await userEvent.type(screen.getByLabelText('Host'), 'sftp.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Read the host key' }));
    expect(await screen.findByRole('button', { name: 'Accept this host key' })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Host'), 'x');
    expect(screen.queryByRole('button', { name: 'Accept this host key' })).not.toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Host'), '{Backspace}');
    await userEvent.type(screen.getByLabelText('Port'), '2');
    expect(screen.queryByRole('button', { name: 'Accept this host key' })).not.toBeInTheDocument();
  });

  it('shows why the host key could not be read', async () => {
    stubApi({ [HOSTKEY]: [502, { code: 'unreachable', message: 'The server cannot be reached.' }] });
    renderForm();
    await userEvent.click(screen.getByLabelText(/^SFTP/));
    expect(screen.getByRole('button', { name: 'Read the host key' })).toBeDisabled();
    await userEvent.type(screen.getByLabelText('Host'), 'sftp.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Read the host key' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('The server cannot be reached.');
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('offline')));
    await userEvent.click(screen.getByRole('button', { name: 'Read the host key' }));
    expect(await screen.findByText(/appliance cannot be reached/i)).toBeInTheDocument();
  });

  it('gives up a host key scan that hangs only after the longer limit of a scan', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout'] });
    stubApi({ [HOSTKEY]: hang });
    renderForm();
    await userEvent.click(screen.getByLabelText(/^SFTP/));
    await userEvent.type(screen.getByLabelText('Host'), 'sftp.example.test');
    const scan = screen.getByRole('button', { name: 'Read the host key' });
    await userEvent.click(scan);
    await vi.advanceTimersByTimeAsync(requestTimeoutMs);
    expect(scan).toBeDisabled();
    await vi.advanceTimersByTimeAsync(slowRequestTimeoutMs - requestTimeoutMs);
    expect(await screen.findByText(/appliance cannot be reached/i)).toBeInTheDocument();
    expect(scan).toBeEnabled();
  });

  it('gives up a save that hangs and enables the button again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout'] });
    stubApi({ 'PUT /appliance/api/v1/backups/targets/offsite': hang });
    renderForm();
    await userEvent.type(screen.getByLabelText('Name'), 'offsite');
    const save = screen.getByRole('button', { name: 'Add the target' });
    await userEvent.click(save);
    expect(save).toBeDisabled();
    await vi.advanceTimersByTimeAsync(requestTimeoutMs);
    expect(await screen.findByText(/appliance cannot be reached/i)).toBeInTheDocument();
    expect(save).toBeEnabled();
  });

  it('adds an NFS target with version 4.1 and says which uid writes', async () => {
    const { sent } = stubApi({ 'PUT /appliance/api/v1/backups/targets/nas': [202] });
    renderForm();
    await userEvent.click(screen.getByLabelText(/^NFS/));
    expect(screen.getByText(/writable by uid and gid 65532/)).toBeInTheDocument();
    expect(screen.getByLabelText('NFS version')).toHaveValue('4.1');
    await userEvent.type(screen.getByLabelText('Name'), 'nas');
    await userEvent.type(screen.getByLabelText('Server'), 'nas.example.test');
    await userEvent.type(screen.getByLabelText('Export'), '/exports');
    await userEvent.selectOptions(screen.getByLabelText('NFS version'), '4.2');
    await userEvent.clear(screen.getByLabelText('Schedule'));
    await userEvent.type(screen.getByLabelText('Schedule'), '0 3 * * 1-5');
    await userEvent.clear(screen.getByLabelText('Backups kept'));
    await userEvent.type(screen.getByLabelText('Backups kept'), '30');
    await userEvent.click(screen.getByRole('button', { name: 'Add the target' }));
    expect(sent['PUT /appliance/api/v1/backups/targets/nas']).toEqual([{
      type: 'nfs', schedule: '0 3 * * 1-5', timeZone: browserTimeZone(), retention: 30,
      nfs: { server: 'nas.example.test', export: '/exports', version: '4.2' },
    }]);
  });

  it('edits a target with its secrets blank, keeping the stored ones', async () => {
    const { sent } = stubApi({ 'PUT /appliance/api/v1/backups/targets/offsite': [202] });
    const editing: Target = {
      name: 'offsite', type: 's3', schedule: '0 4 * * *', timeZone: 'UTC', retention: 5,
      s3: { endpoint: 'https://s3.example.test', bucket: 'ilm', region: 'eu' },
      secretsSet: { ...noSecrets, accessKey: true, secretKey: true, ca: true }, health: { name: 'offsite', warning: false },
    };
    const { onSaved, onCancel } = renderForm(editing);
    expect(screen.getByLabelText('Name')).toHaveAttribute('readonly');
    expect(screen.getByLabelText('Access key')).toHaveValue('');
    expect(screen.getByLabelText('Secret key')).toHaveValue('');
    expect(screen.getAllByText('Leave blank to keep the stored value.')).toHaveLength(2);
    expect(screen.getByText('Leave blank to keep the stored CA bundle.')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Save the target' }));
    expect(onSaved).toHaveBeenCalled();
    expect(sent['PUT /appliance/api/v1/backups/targets/offsite']).toEqual([{
      type: 's3', schedule: '0 4 * * *', timeZone: 'UTC', retention: 5,
      s3: { endpoint: 'https://s3.example.test', bucket: 'ilm', region: 'eu' },
    }]);
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalled();
  });

  it('asks for the secrets again when they would go to another server', async () => {
    renderForm({
      name: 'offsite', type: 's3', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      s3: { endpoint: 'https://s3.example.test', bucket: 'ilm' },
      secretsSet: { ...noSecrets, accessKey: true, secretKey: true, ca: true }, health: { name: 'offsite', warning: false },
    });
    await userEvent.clear(screen.getByLabelText('Bucket'));
    await userEvent.type(screen.getByLabelText('Bucket'), 'other');
    expect(screen.getAllByText('Leave blank to keep the stored value.')).toHaveLength(2);
    expect(screen.getByLabelText('Access key')).not.toBeRequired();
    await userEvent.type(screen.getByLabelText('Endpoint'), 'x');
    expect(screen.queryByText('Leave blank to keep the stored value.')).not.toBeInTheDocument();
    expect(screen.getAllByText(enterAgainHint)).toHaveLength(2);
    expect(screen.getByLabelText('Access key')).toBeRequired();
    expect(screen.getByLabelText('Secret key')).toBeRequired();
    expect(screen.getByText('Leave blank to keep the stored CA bundle.')).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/^SFTP/));
    expect(screen.getByLabelText('Password')).toBeRequired();
  });

  it('asks for the SFTP secret again for another host, port or user', async () => {
    renderForm({
      name: 'sftp', type: 'sftp', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      sftp: { host: 'sftp.example.test', port: 22, path: '/b', user: 'ilm', hostKey: fingerprint },
      secretsSet: { ...noSecrets, password: true }, health: { name: 'sftp', warning: false },
    });
    await userEvent.type(screen.getByLabelText('Directory'), 'x');
    expect(screen.getByLabelText('Password')).not.toBeRequired();
    for (const label of ['Host', 'Port', 'User name']) {
      await userEvent.type(screen.getByLabelText(label), '1');
      expect(screen.getByLabelText('Password')).toBeRequired();
      await userEvent.type(screen.getByLabelText(label), '{Backspace}');
      expect(screen.getByLabelText('Password')).not.toBeRequired();
    }
  });

  it('removes the stored CA bundle only when asked to', async () => {
    const { sent } = stubApi({ 'PUT /appliance/api/v1/backups/targets/offsite': [202] });
    renderForm({
      name: 'offsite', type: 's3', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      s3: { endpoint: 'https://s3.example.test', bucket: 'ilm' },
      secretsSet: { ...noSecrets, accessKey: true, secretKey: true, ca: true }, health: { name: 'offsite', warning: false },
    });
    await userEvent.click(screen.getByLabelText(/Remove the stored CA bundle/));
    expect(screen.queryByLabelText('CA bundle (optional)')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Save the target' }));
    expect(sent['PUT /appliance/api/v1/backups/targets/offsite']).toEqual([{
      type: 's3', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      s3: { endpoint: 'https://s3.example.test', bucket: 'ilm', removeCa: true },
    }]);
  });

  it('offers no CA removal without a stored CA bundle', () => {
    renderForm();
    expect(screen.queryByLabelText(/Remove the stored CA bundle/)).not.toBeInTheDocument();
  });

  it('edits an SFTP target with its accepted key and a private key in place of the password', async () => {
    const { sent } = stubApi({ 'PUT /appliance/api/v1/backups/targets/sftp': [202] });
    renderForm({
      name: 'sftp', type: 'sftp', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      sftp: { host: 'sftp.example.test', port: 22, path: '/b', user: 'ilm', hostKey: fingerprint },
      secretsSet: { ...noSecrets, password: true }, health: { name: 'sftp', warning: false },
    });
    expect(screen.getByText(fingerprint)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Read the host key again' })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Private key'), 'KEY');
    await userEvent.click(screen.getByRole('button', { name: 'Save the target' }));
    expect(sent['PUT /appliance/api/v1/backups/targets/sftp']).toEqual([{
      type: 'sftp', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      sftp: { host: 'sftp.example.test', port: 22, path: '/b', user: 'ilm', hostKey: fingerprint, privateKey: 'KEY' },
    }]);
  });

  it('edits an NFS target', async () => {
    renderForm({
      name: 'nas', type: 'nfs', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
      nfs: { server: 'nas.example.test', export: '/exports', path: 'ilm', version: '3' },
      secretsSet: noSecrets, health: { name: 'nas', warning: false },
    });
    expect(screen.getByLabelText('Server')).toHaveValue('nas.example.test');
    expect(screen.getByLabelText('Directory (optional)')).toHaveValue('ilm');
    expect(screen.getByLabelText('NFS version')).toHaveValue('3');
  });
});
