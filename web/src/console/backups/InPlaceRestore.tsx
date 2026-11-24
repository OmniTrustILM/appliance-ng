import { useState } from 'react';
import { api, timeoutSignal } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button, buttonClass } from '../../components/Button';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { basePath } from '../../base';
import { KitForm } from '../../restore/KitForm';
import { inspectBackup, refusalOf, restoreTimeoutMs } from '../../restore/restoreApi';
import { attempt } from '../../setup/failure';

type Backup = components['schemas']['BackupRecord'];

/** Asks whether to restore backup in place: everything since its time is lost. */
export function RestoreConfirmation({ backup, onConfirm, onCancel }: Readonly<{ backup: Backup; onConfirm: () => void; onCancel: () => void }>) {
  return (
    <ConfirmDialog title="Restore this backup?" confirmLabel="Restore" onConfirm={onConfirm} onCancel={onCancel}>
      {`The appliance and ILM go back to the backup of ${new Date(backup.time).toLocaleString()}: everything changed since then is lost, ` +
        "ILM's data and the settings included. The console accounts become the backup's: accounts created since then are removed; " +
        "yours is kept, unless the backup has an account of the same name, which wins. " +
        'The node keeps its network settings. ILM is stopped while the backup is restored.'}
    </ConfirmDialog>
  );
}

/** Restores a local backup in place with the recovery kit, once its restore was confirmed. */
export function InPlaceRestore({ backup, onCancel }: Readonly<{ backup: Backup; onCancel: () => void }>) {
  const [started, setStarted] = useState(false);

  async function restore(kit: File, passphrase: string) {
    const checked = await attempt(() => inspectBackup('local', backup.name, kit, passphrase));
    const refused = refusalOf(checked);
    if (refused || !checked?.data) return refused;
    const body = { inspection: checked.data.inspection, inPlace: true, confirm: true };
    const failed = refusalOf(await attempt(() => api.POST('/restore/start', { body, signal: timeoutSignal(restoreTimeoutMs) })));
    if (!failed) setStarted(true);
    return failed;
  }

  if (started) {
    return (
      <Alert tone="success">
        The restore of {backup.name} started.{' '}
        <a className={buttonClass('secondary')} href={`${basePath}/restore`}>Follow the restore</a>
      </Alert>
    );
  }
  return (
    <div className="mt-5 border-t border-divider pt-5">
      <h3 className="mb-1.5 font-semibold">Restore {backup.name}</h3>
      <p className="mb-4 text-content-muted">The recovery kit and its passphrase open the backup; the whole backup is checked before anything changes.</p>
      <KitForm label={`Restore ${backup.name}`} submitLabel="Restore the backup" onSubmit={restore}>
        <Button variant="secondary" onClick={onCancel}>Cancel</Button>
      </KitForm>
    </div>
  );
}
