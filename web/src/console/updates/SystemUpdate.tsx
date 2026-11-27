import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { systemStepOrder, systemStepText, updateFailureText } from './reasons';

type Bundle = components['schemas']['UpdateBundle'];
type Progress = components['schemas']['UpdateProgress'];

/** The step after which the way back is a restore onto a fresh appliance. */
const pointOfNoReturnStep = 'kubernetes';

const freshAppliance = 'restore the backup taken first onto a fresh appliance of the previous release';

/** The steps of a system update: those done, the one it is at, and the point of no return. */
function Steps({ update }: Readonly<{ update: Progress }>) {
  const at = update.step === 'done' ? systemStepOrder.length : systemStepOrder.indexOf(update.step as (typeof systemStepOrder)[number]);
  return (
    <ol aria-label="Steps of the system update" className="mb-3 mt-0 list-none p-0 text-content-muted">
      {systemStepOrder.map((step, i) => (
        <li key={step} aria-current={i === at ? 'step' : undefined} className={i === at ? 'font-semibold text-content' : undefined}>
          {i < at ? '✓ ' : ''}{systemStepText(step)}
          {step === pointOfNoReturnStep && ' (point of no return)'}
        </li>
      ))}
    </ol>
  );
}

/** The way back once an update is past the point of no return. */
function PointOfNoReturn({ update }: Readonly<{ update: Progress }>) {
  if (!update.pointOfNoReturn) return null;
  return (
    <Alert tone="warning">
      The update is past the point of no return: it can no longer be undone. The way back is to {freshAppliance}; data changed since that backup is lost then.
    </Alert>
  );
}

/** The latest system update: the steps and the one it is at, how it completed, or why it failed. */
export function SystemStatus({ update }: Readonly<{ update: Progress }>) {
  const backup = update.backup ? ` The backup ${update.backup} was taken before it.` : '';
  if (update.step === 'done') return <Alert tone="success">The system was updated to {update.to}.</Alert>;
  if (update.message) {
    // A failed health check is recorded at the rollback of Talos it led to.
    const step = update.message === 'healthCheckFailed' ? 'health' : update.step;
    return (
      <>
        <Alert tone="error">
          The update to {update.to} failed while {systemStepText(step)}. {updateFailureText(update.message)}{backup}
        </Alert>
        <PointOfNoReturn update={update} />
      </>
    );
  }
  return (
    <>
      <Alert tone="info" busy>
        Updating the system from {update.from} to {update.to}: {systemStepText(update.step)}…{backup}
      </Alert>
      <Steps update={update} />
      <PointOfNoReturn update={update} />
    </>
  );
}

type SystemConfirmProps = Readonly<{ bundle: Bundle; onConfirm: () => void; onCancel: () => void }>;

/** Asks before a system update, which takes the appliance down for about ten minutes. */
export function SystemConfirm({ bundle, onConfirm, onCancel }: SystemConfirmProps) {
  return (
    <ConfirmDialog title={`Install system ${bundle.version}?`} confirmLabel="Install and restart" onConfirm={onConfirm} onCancel={onCancel}>
      <p className="m-0 mb-2">
        Expect about 10 minutes of downtime: the manager restarts, then the node reboots. The console is back 2 to 3 minutes after the reboot; the
        add-ons and ILM need 3 to 5 minutes more. This page waits for the appliance and asks you to sign in again once it is back.
      </p>
      <p className="m-0 mb-2">
        The appliance takes a backup to every backup target first; the update starts only once it has completed, and stops if the backup fails.
      </p>
      <p className="m-0">
        If the node does not come back healthy on the new Talos, Talos is rolled back and the appliance runs the previous release. Once the node is
        healthy on the new Talos, the point of no return, there is no undo: the way back is to {freshAppliance}, and data changed after the backup is
        lost then.
      </p>
    </ConfirmDialog>
  );
}
