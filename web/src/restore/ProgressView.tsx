import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { cx } from '../components/cx';
import { attempt } from '../setup/failure';
import { KitForm } from './KitForm';
import { refusalOf, resumeRestore } from './restoreApi';
import { currentStep, stepLabel, stepsOf, type Progress } from './steps';

const reasons: Record<NonNullable<Progress['message']>, string> = {
  failed: 'a step failed; the manager log has the details',
  timeout: 'a step did not finish in time',
};

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
  const reached = steps.findIndex((s) => s.step === at);
  const done = progress.step === 'done';
  const resumable = Boolean(progress.message) || progress.step === 'waitingForKit';
  const restarted = reached >= steps.findIndex((s) => s.step === 'stop') ? ' ILM was started again.' : '';

  async function resume(kit: File, passphrase: string) {
    const refused = refusalOf(await attempt(() => resumeRestore(kit, passphrase)));
    if (!refused) onResumed();
    return refused;
  }

  return (
    <>
      <h2 className="mb-3 text-lg font-semibold">{done ? `Restored ${progress.backup}` : `Restoring ${progress.backup}`}</h2>
      {done && <Alert tone="success">The restore finished.</Alert>}
      {progress.warning && <Alert tone="warning">{progress.warning}</Alert>}
      {progress.message && (
        <Alert tone="error">{`The restore stopped at "${stepLabel(at)}": ${reasons[progress.message]}.${restarted}`}</Alert>
      )}
      {progress.step === 'waitingForKit' && (
        <Alert tone="warning">
          {`The manager restarted during the restore, and the backup key lived only in its memory. Give the recovery kit and its passphrase again to go on at "${stepLabel(at)}".`}
        </Alert>
      )}
      <ol className="mb-5 list-none p-0">
        {steps.map((s, i) => (
          <li key={s.step} aria-current={i === reached && !done ? 'step' : undefined}
            className={cx('border-b border-divider py-2 text-sm last:border-b-0', (done || i < reached) && 'text-content-muted', i === reached && !done && 'font-semibold')}>
            {s.label}
          </li>
        ))}
      </ol>
      {resumable && (
        <KitForm label="Resume the restore" submitLabel="Resume the restore" onSubmit={resume}>
          {fresh && progress.message && <Button variant="secondary" onClick={onStartOver}>Choose another backup</Button>}
        </KitForm>
      )}
    </>
  );
}
