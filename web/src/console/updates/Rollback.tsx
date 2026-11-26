import { useState } from 'react';
import { api, timeoutSignal } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { KitForm } from '../../restore/KitForm';
import { kitForm, refusalOf, restoreTimeoutMs, type Refusal } from '../../restore/restoreApi';
import { attempt } from '../../setup/failure';
import { rollbackRefusalText } from './reasons';

/** Starts the rollback of the last platform update with the recovery kit, which opens the backup taken before it. */
function startRollback(kit: File, passphrase: string) {
  return api.POST('/updates/rollback', { body: kitForm(kit, passphrase, { confirm: 'true' }) as never, signal: timeoutSignal(restoreTimeoutMs) });
}

const consequences = 'Data changed since that backup is lost.';

type RollbackCardProps = Readonly<{ backup?: string; disabled: boolean; onStarted: () => void }>;

/**
 * Rolls the last platform update back after a confirmation: ILM is installed again at the version that ran before and
 * backup, the backup taken before the update, is restored with the recovery kit and its passphrase.
 */
export function RollbackCard({ backup, disabled, onStarted }: RollbackCardProps) {
  const [asking, setAsking] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const taken = backup ? `the backup ${backup} taken before it` : 'the backup taken before it';

  async function rollBack(kit: File, passphrase: string): Promise<Refusal | undefined> {
    const result = await attempt(() => startRollback(kit, passphrase));
    const refused = refusalOf(result);
    if (!refused) {
      setConfirmed(false);
      onStarted();
      return undefined;
    }
    return { ...refused, message: rollbackRefusalText(result?.error?.code) ?? refused.message };
  }

  return (
    <section aria-label="Roll back the last update">
      <Card>
        <h2 className="mb-1.5 text-lg font-semibold">Roll back the last update</h2>
        <p className="mb-5 text-content-muted">
          A rollback installs ILM again at the version that ran before the update and restores {taken}. {consequences} It needs the recovery kit and its
          passphrase.
        </p>
        {confirmed ? (
          <KitForm label="Roll back the platform update" submitLabel="Roll back" onSubmit={rollBack}>
            <Button variant="secondary" onClick={() => setConfirmed(false)}>Cancel</Button>
          </KitForm>
        ) : (
          <Button disabled={disabled} onClick={() => setAsking(true)}>Roll back</Button>
        )}
        {asking && (
          <ConfirmDialog title="Roll back the platform update?" confirmLabel="Roll back"
            onConfirm={() => { setAsking(false); setConfirmed(true); }} onCancel={() => setAsking(false)}>
            ILM is stopped, installed again at the version that ran before the update, and {taken} is restored. {consequences}
          </ConfirmDialog>
        )}
      </Card>
    </section>
  );
}

/** Why the last platform update cannot be rolled back, when that is worth telling. */
export function RollbackUnavailable({ text }: Readonly<{ text: string }>) {
  return <div className="mt-6"><Alert tone="warning">{text}</Alert></div>;
}
