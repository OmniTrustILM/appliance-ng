import { useState, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { Field } from '../../components/Field';
import { FileDropField } from '../../components/FileDropField';
import { useIssue } from '../access/useIssue';
import { PassphraseFields, passphrasesAgree } from './PassphraseFields';

export const kitFileName = 'ilm-appliance-recovery-kit.tar.age';

type OnIssued = Readonly<{ onIssued: (filename: string) => void }>;

/** Calls onIssued once the passphrase fields are cleared, so that no passphrase stays on screen. */
function clearing(onIssued: (filename: string) => void, ...clear: ((value: string) => void)[]) {
  return (filename: string) => {
    for (const set of clear) set('');
    onIssued(filename);
  };
}

/** Issues the first kit with a new backup key and downloads it. */
export function IssueForm({ onIssued }: OnIssued) {
  const [passphrase, setPassphrase] = useState('');
  const [repeat, setRepeat] = useState('');
  const { busy, error, fieldErrors, issue } = useIssue(kitFileName, clearing(onIssued, setPassphrase, setRepeat));

  function submit(event: SubmitEvent) {
    event.preventDefault();
    issue(() => api.POST('/recovery-kit', { body: { passphrase }, parseAs: 'blob' }));
  }

  return (
    <form aria-label="Issue the recovery kit" onSubmit={submit} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <PassphraseFields label="Passphrase" name="passphrase" value={passphrase} repeat={repeat}
        onChange={setPassphrase} onRepeatChange={setRepeat} error={fieldErrors.passphrase} />
      <Button type="submit" busy={busy} disabled={!passphrasesAgree(passphrase, repeat)}>Issue the recovery kit</Button>
    </form>
  );
}

/** Issues the kit again from the current one: the same backup key, a new passphrase and new break-glass access. */
export function ReissueForm({ onIssued }: OnIssued) {
  const [kit, setKit] = useState<File>();
  const [current, setCurrent] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [repeat, setRepeat] = useState('');
  const { busy, error, fieldErrors, issue } = useIssue(kitFileName, clearing(onIssued, setCurrent, setPassphrase, setRepeat));

  function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!kit) return;
    const form = new FormData();
    form.append('kit', kit);
    form.append('passphrase', current);
    form.append('newPassphrase', passphrase);
    issue(() => api.POST('/recovery-kit/reissue', { body: form as never, parseAs: 'blob' }));
  }

  return (
    <form aria-label="Issue the kit again" onSubmit={submit} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      {fieldErrors.kit && <Alert tone="error">{fieldErrors.kit}</Alert>}
      <FileDropField label="Current kit" required file={kit} onChange={setKit} hint="The latest kit this appliance issued." />
      <Field label="Current passphrase" name="current-passphrase" type="password" autoComplete="current-password"
        value={current} onChange={(event) => setCurrent(event.target.value)} error={fieldErrors.passphrase} />
      <PassphraseFields label="New passphrase" name="new-passphrase" value={passphrase} repeat={repeat}
        onChange={setPassphrase} onRepeatChange={setRepeat} error={fieldErrors.newPassphrase} />
      <Button type="submit" busy={busy} disabled={!kit || current === '' || !passphrasesAgree(passphrase, repeat)}>
        Issue the kit again
      </Button>
    </form>
  );
}

/** Replaces a lost kit with one holding a new backup key, after a warning about the backups made so far. */
export function ReplaceForm({ onIssued }: OnIssued) {
  const [passphrase, setPassphrase] = useState('');
  const [repeat, setRepeat] = useState('');
  const [asking, setAsking] = useState(false);
  const { busy, error, fieldErrors, issue } = useIssue(kitFileName, clearing(onIssued, setPassphrase, setRepeat));

  function submit(event: SubmitEvent) {
    event.preventDefault();
    setAsking(true);
  }

  function replace() {
    setAsking(false);
    issue(() => api.POST('/recovery-kit/replace', { body: { passphrase, confirm: true }, parseAs: 'blob' }));
  }

  return (
    <form aria-label="Replace a lost kit" onSubmit={submit} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <PassphraseFields label="New passphrase" name="replace-passphrase" value={passphrase} repeat={repeat}
        onChange={setPassphrase} onRepeatChange={setRepeat} error={fieldErrors.passphrase} />
      <Button type="submit" variant="secondary" busy={busy} disabled={!passphrasesAgree(passphrase, repeat)}>Replace the kit</Button>
      {asking && (
        <ConfirmDialog title="Replace the recovery kit?" confirmLabel="Replace the kit" onConfirm={replace} onCancel={() => setAsking(false)}>
          The new kit holds a new backup key. Backups made so far stay readable only with the old kit; make a new
          backup once the new kit is stored. Only the old kubeconfig is revoked: the old talosconfig keeps node
          access until its expiry date, because Talos cannot revoke it.
        </ConfirmDialog>
      )}
    </form>
  );
}
