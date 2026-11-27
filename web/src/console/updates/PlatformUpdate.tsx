import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { basePath } from '../../base';
import { SystemStatus } from './SystemUpdate';
import { rollbackFailureText, rollbackStepText, stepText, updateFailureText } from './reasons';

type Bundle = components['schemas']['UpdateBundle'];
type Progress = components['schemas']['UpdateProgress'];

/** The steps at which an update, or the restore of a rollback, has ended or waits for the administrator. */
const settled = new Set(['done', 'abandoned', 'waitingForKit']);

/** Whether an update or a rollback runs on its own: it neither ended nor failed nor waits for the kit. */
export function updating(update?: Progress) {
  return !!update && !settled.has(update.step ?? '') && !update.message;
}

/** Whether version, X.Y.Z with an optional leading v, is newer than than; false when either is missing. */
export function newer(version?: string, than?: string) {
  if (!version || !than) return false;
  const parts = (v: string) => v.replace(/^v/, '').split('.').map((p) => Number.parseInt(p, 10) || 0);
  const [a, b] = [parts(version), parts(than)];
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    if ((a[i] ?? 0) !== (b[i] ?? 0)) return (a[i] ?? 0) > (b[i] ?? 0);
  }
  return false;
}

const restorePage = <a href={`${basePath}/restore`}>Restore page</a>;

/** The latest rollback: the step of the restore that does it, how it ended, or why it stopped. */
function RollbackStatus({ update }: Readonly<{ update: Progress }>) {
  if (update.step === 'done') {
    return <Alert tone="success">The platform was rolled back to {update.to}, with the data of the backup {update.backup}.</Alert>;
  }
  if (update.step === 'abandoned') {
    return <Alert tone="warning">The rollback was abandoned. The appliance keeps whatever state it reached; the update can be rolled back again.</Alert>;
  }
  if (update.step === 'waitingForKit') {
    return (
      <Alert tone="warning">
        The manager restarted during the rollback, and the backup key lived only in its memory. Give the recovery kit again on the {restorePage} to go on.
      </Alert>
    );
  }
  if (update.message) {
    return (
      <Alert tone="error">
        The rollback stopped while {rollbackStepText(update.step)}: {rollbackFailureText(update.message)}. Resume or abandon it on the {restorePage}.
      </Alert>
    );
  }
  return (
    <Alert tone="info" busy>
      Rolling back the platform from {update.from} to {update.to}: {rollbackStepText(update.step)}…
    </Alert>
  );
}

/** The latest update or rollback: the step it is at, how it completed, or why it failed. */
export function UpdateStatus({ update }: Readonly<{ update: Progress }>) {
  if (update.kind === 'rollback') return <RollbackStatus update={update} />;
  if (update.kind === 'system') return <SystemStatus update={update} />;
  const backup = update.backup ? ` The backup ${update.backup} was taken before it.` : '';
  if (update.step === 'done') return <Alert tone="success">The platform was updated to {update.to}.</Alert>;
  if (update.message) {
    return (
      <Alert tone="error">
        The update to {update.to} failed while {stepText(update.step)}. {updateFailureText(update.message)}{backup}
      </Alert>
    );
  }
  return (
    <Alert tone="info" busy>
      Updating the platform from {update.from} to {update.to}: {stepText(update.step)}…{backup}
    </Alert>
  );
}

type InstallConfirmProps = Readonly<{
  bundle: Bundle;
  operator?: string;
  externalDatabase?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}>;

/** Why a platform update from bundle cannot be rolled back, or false when it can. */
function noRollback(bundle: Bundle, operator?: string, externalDatabase?: boolean) {
  if (externalDatabase) return 'The platform uses an external database, which a rollback cannot take back to the previous version.';
  if (!operator) return 'The ILM operator that runs is not known, so this bundle may install a newer one, which is never downgraded.';
  return newer(bundle.operator, operator) && 'This bundle installs a newer ILM operator, which is never downgraded.';
}

/**
 * Asks before a platform update, which takes a backup first. The way back is a rollback, unless the platform uses an
 * external database, or the bundle brings a newer ILM operator than operator, the one that runs, or that one is not
 * known: a newer operator is never downgraded. The way back is then a restore onto a fresh appliance. Either loses
 * the data changed since the backup.
 */
export function InstallConfirm({ bundle, operator, externalDatabase, onConfirm, onCancel }: InstallConfirmProps) {
  const noUndo = noRollback(bundle, operator, externalDatabase);
  return (
    <ConfirmDialog title={`Install platform ${bundle.version}?`} confirmLabel="Install" onConfirm={onConfirm} onCancel={onCancel}>
      {bundle.ilm && <p className="m-0 mb-2">The platform moves to ILM {bundle.ilm}.</p>}
      <p className="m-0 mb-2">
        The appliance takes a backup to every backup target first; the update starts only once it has completed. The update stops if the backup fails.
      </p>
      {noUndo ? (
        <p className="m-0">
          {noUndo} There is no undo: the way back is to restore that backup onto a fresh appliance, and data changed after the backup is lost then.
        </p>
      ) : (
        <p className="m-0">
          There is no undo, but the update can be rolled back: ILM is installed again at the previous version and that backup is restored. Data changed
          after the backup is lost then. A rollback needs the recovery kit and its passphrase.
        </p>
      )}
    </ConfirmDialog>
  );
}
