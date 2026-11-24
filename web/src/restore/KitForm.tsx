import { useState, type ReactNode, type SubmitEvent } from 'react';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Field } from '../components/Field';
import { FileDropField } from '../components/FileDropField';
import type { Refusal } from './restoreApi';

type KitFormProps = Readonly<{
  label: string;
  submitLabel: string;
  /** Sends the kit and passphrase; a refusal is shown, at the passphrase when it concerns that field. */
  onSubmit: (kit: File, passphrase: string) => Promise<Refusal | undefined>;
  children?: ReactNode;
}>;

/** Asks for the recovery kit and its passphrase; the passphrase is cleared after each attempt. The other actions in
 * children wait while an attempt runs. */
export function KitForm({ label, submitLabel, onSubmit, children }: KitFormProps) {
  const [kit, setKit] = useState<File>();
  const [passphrase, setPassphrase] = useState('');
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<Refusal>();

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!kit) return;
    setBusy(true);
    setRefusal(undefined);
    const refused = await onSubmit(kit, passphrase);
    setPassphrase('');
    setBusy(false);
    setRefusal(refused);
  }

  const atPassphrase = refusal?.field === 'passphrase';
  return (
    <form aria-label={label} onSubmit={(event) => void submit(event)} noValidate>
      {refusal && !atPassphrase && <Alert tone="error">{refusal.message}</Alert>}
      <FileDropField label="Recovery kit" required file={kit} onChange={setKit} hint="The kit whose backup key the backup is encrypted to." />
      <Field label="Passphrase" name="kit-passphrase" type="password" autoComplete="off" value={passphrase}
        onChange={(event) => setPassphrase(event.target.value)} error={atPassphrase ? refusal.message : undefined} />
      <div className="flex flex-col gap-2 sm:flex-row">
        <Button type="submit" busy={busy} disabled={!kit || passphrase === ''}>{submitLabel}</Button>
        <fieldset disabled={busy} className="contents">{children}</fieldset>
      </div>
    </form>
  );
}
