import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { UpdatesPage, runningRefreshMs } from './UpdatesPage';

const UPDATES = 'GET /appliance/api/v1/updates';
const ROLLBACK = 'POST /appliance/api/v1/updates/rollback';

const finished = { kind: 'platform', from: '0.1.0', to: '0.1.1', step: 'done', backup: 'pre-update.ilmbackup', startedAt: '2026-11-24T10:00:00Z', pointOfNoReturn: true };

function updates(over: object = {}) {
  return { available: [], loaded: [], update: finished, rollback: { available: true }, ...over };
}

function rollback(over: object = {}) {
  return { kind: 'rollback', from: '0.1.1', to: '0.1.0', step: 'stop', backup: 'pre-update.ilmbackup', startedAt: '2026-11-24T11:00:00Z', pointOfNoReturn: false, ...over };
}

/** Opens the rollback, confirms it and fills the kit form. */
async function fillRollback() {
  await userEvent.click(await screen.findByRole('button', { name: 'Roll back' }));
  const dialog = screen.getByRole('dialog', { name: /roll back the platform update/i });
  await userEvent.click(within(dialog).getByRole('button', { name: 'Roll back' }));
  const form = screen.getByRole('form', { name: 'Roll back the platform update' });
  await userEvent.upload(within(form).getByLabelText(/recovery kit/i), new File(['kit'], 'kit.tar.age'));
  await userEvent.type(within(form).getByLabelText('Passphrase'), 'the kit passphrase');
  return form;
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('UpdatesPage rollback', () => {
  it('offers Roll back while the last platform update can be rolled back, and says what it does', async () => {
    const { sent } = stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    const card = await screen.findByRole('region', { name: 'Roll back the last update' });
    expect(card).toHaveTextContent(/installs ILM again at the version that ran before the update/i);
    expect(card).toHaveTextContent(/restores the backup pre-update\.ilmbackup taken before it/i);
    expect(card).toHaveTextContent(/data changed since that backup is lost/i);
    expect(card).toHaveTextContent(/needs the recovery kit and its passphrase/i);
    await userEvent.click(within(card).getByRole('button', { name: 'Roll back' }));
    const dialog = screen.getByRole('dialog', { name: /roll back the platform update/i });
    expect(dialog).toHaveTextContent(/data changed since that backup is lost/i);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('form', { name: 'Roll back the platform update' })).not.toBeInTheDocument();
    expect(sent[ROLLBACK]).toBeUndefined();
  });

  it.each([
    [{ available: false, reason: 'noPreUpdateBackup' }],
    [undefined],
  ])('offers no rollback for %o', async (offer) => {
    stubApi({ [UPDATES]: [200, updates({ rollback: offer })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(/the platform was updated to 0\.1\.1/i)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Roll back' })).not.toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Roll back the last update' })).not.toBeInTheDocument();
  });

  it.each([
    ['notNeeded', /failed before it asked for the new ILM version.*ILM operator may already run/i],
    ['externalDatabase', /external database, which a rollback cannot restore/i],
    ['operatorChanged', /newer ILM operator, which is never downgraded.*fresh appliance/i],
    ['noPreviousBundle', /platform bundle that ran before it is no longer on the appliance.*fresh appliance/i],
    ['upgradeInProgress', /operator is upgrading the platform/i],
  ])('explains why the update cannot be rolled back: %s', async (reason, text) => {
    stubApi({ [UPDATES]: [200, updates({ rollback: { available: false, reason } })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Roll back' })).not.toBeInTheDocument();
  });

  it('rolls back with the kit and passphrase and follows the rollback', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let update: object = finished;
    let offer: object = { available: true };
    const { sent } = stubApi({
      [UPDATES]: () => [200, updates({ update, rollback: offer })],
      [ROLLBACK]: () => {
        update = rollback();
        offer = { available: false, reason: 'restoreRunning' };
        return [202];
      },
    });
    render(<UpdatesPage />);
    const form = await fillRollback();
    await userEvent.click(within(form).getByRole('button', { name: 'Roll back' }));
    expect(sent[ROLLBACK]).toEqual([expect.objectContaining({ passphrase: 'the kit passphrase', confirm: 'true' })]);
    expect(await screen.findByText(/rolling back the platform from 0\.1\.1 to 0\.1\.0: stopping ILM/i)).toBeInTheDocument();
    expect(screen.queryByRole('form', { name: 'Roll back the platform update' })).not.toBeInTheDocument();
    for (const [step, text] of [
      ['uninstall', /uninstalling the platform/i],
      ['reinstall', /installing ILM again at the previous version/i],
      ['database', /loading the backup's database/i],
    ] as const) {
      update = rollback({ step });
      await act(async () => { await vi.advanceTimersByTimeAsync(runningRefreshMs); });
      expect(await screen.findByText(text)).toBeInTheDocument();
    }
    update = rollback({ step: 'done' });
    await act(async () => { await vi.advanceTimersByTimeAsync(runningRefreshMs); });
    expect(await screen.findByText(/the platform was rolled back to 0\.1\.0/i)).toBeInTheDocument();
  });

  it.each([
    [{ step: 'database', message: 'timeout' }, /rollback stopped while loading the backup's database: a step did not finish in time/i],
    [{ step: 'uninstall', message: 'failed' }, /rollback stopped while uninstalling the platform: a step failed/i],
    [{ step: 'waitingForKit' }, /manager restarted during the rollback/i],
    [{ step: 'abandoned' }, /rollback was abandoned/i],
  ])('explains a rollback that stopped: %o', async (over, text) => {
    stubApi({ [UPDATES]: [200, updates({ update: rollback(over), rollback: { available: false, reason: 'restoreRunning' } })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
    if (over.step !== 'abandoned') {
      expect(screen.getByRole('link', { name: 'Restore page' })).toHaveAttribute('href', '/appliance/restore');
    }
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
  });

  it.each([
    [400, 'wrong_passphrase', 'The passphrase does not open the kit.', 'passphrase', /does not open the kit/i],
    [409, 'no_pre_update_backup', 'x', undefined, /no platform update to roll back/i],
    [409, 'no_previous_bundle', 'x', undefined, /ran before the update is no longer on the appliance.*fresh appliance/i],
    [409, 'operator_changed', 'x', undefined, /restore the backup onto a fresh appliance instead/i],
    [409, 'not_needed', 'x', undefined, /failed before it asked for the new ILM version/i],
    [409, 'external_database', 'x', undefined, /external database/i],
    [409, 'foreign_kit', 'x', undefined, /kit does not open the backup/i],
    [409, 'backup_broken', 'x', undefined, /backup taken before the update cannot be restored/i],
    [409, 'update_running', 'x', undefined, /another update or rollback is running/i],
    [409, 'backup_running', 'x', undefined, /a backup is running\. roll back once/i],
    [409, 'restore_running', 'x', undefined, /a restore is running\. roll back once/i],
    [409, 'busy', 'x', undefined, /another operation is running/i],
    [409, 'upgrade_in_progress', 'x', undefined, /operator is upgrading the platform\. roll back once/i],
    [500, 'internal', 'The request failed.', undefined, /the request failed/i],
  ])('explains a rollback refused with %i %s', async (status, code, message, field, text) => {
    stubApi({ [UPDATES]: [200, updates()], [ROLLBACK]: [status, { code, message, field }] });
    render(<UpdatesPage />);
    const form = await fillRollback();
    await userEvent.click(within(form).getByRole('button', { name: 'Roll back' }));
    expect(await within(form).findByText(text)).toBeInTheDocument();
    expect(within(form).getByLabelText('Passphrase')).toHaveValue('');
  });

  it('reports a rollback request that never reached the appliance', async () => {
    stubApi({ [UPDATES]: [200, updates()], [ROLLBACK]: () => Promise.reject(new TypeError('offline')) });
    render(<UpdatesPage />);
    const form = await fillRollback();
    await userEvent.click(within(form).getByRole('button', { name: 'Roll back' }));
    expect(await within(form).findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('closes the kit form on Cancel', async () => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    const form = await fillRollback();
    await userEvent.click(within(form).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('form', { name: 'Roll back the platform update' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Roll back' })).toBeInTheDocument();
  });
});
