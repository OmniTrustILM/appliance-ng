import { useState } from 'react';
import { Link } from 'react-router';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { cx } from '../components/cx';
import { attempt } from '../setup/failure';
import { KitForm } from './KitForm';
import { abandonRestore, refusalOf, resumeRestore } from './restoreApi';
import { currentStep, stepIndex, stepLabel, stepsOf, type Progress } from './steps';

const reasons: Record<NonNullable<Progress['message']>, string> = {
  failed: 'a step failed; the manager log has the details',
  timeout: 'a step did not finish in time',
};

function heading(progress: Progress) {
  if (progress.step === 'done') return `Restored ${progress.backup}`;
  return progress.step === 'abandoned' ? `Abandoned the restore of ${progress.backup}` : `Restoring ${progress.backup}`;
}

type AbandonProps = Readonly<{ onAbandoned: () => void }>;

/** Gives up a failed restore after a confirmation: the appliance keeps whatever state the restore reached. */
function Abandon({ onAbandoned }: AbandonProps) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string>();

  async function abandon() {
    setBusy(true);
    const refused = refusalOf(await attempt(() => abandonRestore()));
    setBusy(false);
    setAsking(false);
    if (refused) return setRefusal(refused.message);
    onAbandoned();
  }

  return (
    <>
      {refusal && <Alert tone="error">{refusal}</Alert>}
      <Button variant="secondary" busy={busy} onClick={() => setAsking(true)}>Abandon the restore</Button>
      {asking && (
        <ConfirmDialog title="Abandon the restore?" confirmLabel="Abandon the restore" onConfirm={() => void abandon()} onCancel={() => setAsking(false)}>
          The appliance keeps whatever state the restore reached, which may be part of the backup and part of what it held before. Backups and the ILM settings go on; the backup file stays.
        </ConfirmDialog>
      )}
    </>
  );
}

type ProgressViewProps = Readonly<{
  progress: Progress;
  /** Whether the appliance is a fresh one waiting for a restore, which may choose another backup. */
  fresh: boolean;
  onResumed: () => void;
  onStartOver: () => void;
}>;

/** The steps of a restore with the one it is at, and the kit form that resumes one waiting for its kit or failed. */
export function ProgressView({ progress, fresh, onResumed, onStartOver }: ProgressViewProps) {
  const at = currentStep(progress);
  const steps = stepsOf(progress);
  const reached = stepIndex(progress, steps);
  const label = steps[reached]?.label ?? stepLabel(at);
  const done = progress.step === 'done';
  const abandoned = progress.step === 'abandoned';
  const resumable = Boolean(progress.message) || progress.step === 'waitingForKit';
  const restarted = reached >= steps.findIndex((s) => s.step === 'stop') ? ' ILM was started again.' : '';

  async function resume(kit: File, passphrase: string) {
    const refused = refusalOf(await attempt(() => resumeRestore(kit, passphrase)));
    if (!refused) onResumed();
    return refused;
  }

  return (
    <>
      <h2 className="mb-3 text-lg font-semibold">{heading(progress)}</h2>
      {done && <Alert tone="success">The restore finished.</Alert>}
      {abandoned && (
        <Alert tone="warning">The restore was abandoned. The appliance keeps whatever state it reached; the backup file stays on the appliance.</Alert>
      )}
      {done && !progress.inPlace && (
        <Alert tone="warning">
          The recovery kit's cluster access belongs to the node the backup was taken on, so backups wait until a new recovery kit is issued and confirmed.{' '}
          <Link to="/recovery-kit">Issue a new recovery kit</Link>
        </Alert>
      )}
      {progress.warning && <Alert tone="warning">{progress.warning}</Alert>}
      {progress.message && (
        <Alert tone="error">{`The restore stopped at "${label}": ${reasons[progress.message]}.${restarted}`}</Alert>
      )}
      {progress.step === 'waitingForKit' && (
        <Alert tone="warning">
          {`The manager restarted during the restore, and the backup key lived only in its memory. Give the recovery kit and its passphrase again to go on at "${label}".`}
        </Alert>
      )}
      {!abandoned && <ol className="mb-5 list-none p-0">
        {steps.map((s, i) => (
          <li key={s.label} aria-current={i === reached && !done ? 'step' : undefined}
            className={cx('border-b border-divider py-2 text-sm last:border-b-0', (done || i < reached) && 'text-content-muted', i === reached && !done && 'font-semibold')}>
            {s.label}
          </li>
        ))}
      </ol>}
      {resumable && (
        <KitForm label="Resume the restore" submitLabel="Resume the restore" onSubmit={resume}>
          {fresh && progress.message && <Button variant="secondary" onClick={onStartOver}>Choose another backup</Button>}
          {progress.message && <Abandon onAbandoned={onResumed} />}
        </KitForm>
      )}
    </>
  );
}
