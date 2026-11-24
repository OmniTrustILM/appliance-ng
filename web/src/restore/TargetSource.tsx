import { useState } from 'react';
import { api, timeoutSignal, slowRequestTimeoutMs } from '../api/client';
import type { components } from '../api/schema';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { ChoiceField } from '../components/ChoiceField';
import { TargetFields } from '../console/backups/TargetFields';
import { updateOf, useTargetValues, type SetValue } from '../console/backups/targetValues';
import { formatBytes } from '../console/shared/format';
import { attempt } from '../setup/failure';
import { refusalOf, restoreTimeoutMs, type Refusal } from './restoreApi';
import { useLatest } from './useLatest';

type RestoreFile = components['schemas']['RestoreFile'];

/** The fields of a refusal that the target form shows at the field. */
const targetField = /^(s3|sftp|nfs)\./;

/**
 * A target entered for the restore, as on the Backups page, and a backup chosen from its listing. The target and
 * its secrets serve the requests of this page only; the restore saves the backup's own targets.
 */
export function TargetSource({ onReady }: Readonly<{ onReady: (name: string) => void }>) {
  const [values, setValue] = useTargetValues();
  const [files, setFiles] = useState<RestoreFile[]>();
  const [chosen, setChosen] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<Refusal>();
  const begin = useLatest();
  const atField = refusal?.field !== undefined && targetField.test(refusal.field);
  const errors = refusal?.field && atField ? { [refusal.field]: refusal.message } : {};

  /** A listing belongs to the target it was read from; one still being read is dropped. */
  const set: SetValue = (key, value) => {
    begin();
    setBusy(false);
    setValue(key, value);
    setFiles(undefined);
    setChosen(undefined);
  };

  async function list() {
    const latest = begin();
    setBusy(true);
    setRefusal(undefined);
    setFiles(undefined);
    setChosen(undefined);
    const result = await attempt(() => api.POST('/restore/list', { body: { target: updateOf(values) }, signal: timeoutSignal(slowRequestTimeoutMs) }));
    if (!latest()) return;
    setBusy(false);
    setRefusal(refusalOf(result));
    if (result?.data) setFiles(result.data.files);
  }

  async function fetchBackup(name: string) {
    const latest = begin();
    setBusy(true);
    setRefusal(undefined);
    const body = { target: updateOf(values), name };
    const refused = refusalOf(await attempt(() => api.POST('/restore/fetch', { body, signal: timeoutSignal(restoreTimeoutMs) })));
    if (!latest()) return;
    setBusy(false);
    if (refused) return setRefusal(refused);
    onReady(name);
  }

  return (
    <>
      {refusal && !atField && <Alert tone="error">{refusal.message}</Alert>}
      <TargetFields values={values} set={set} errors={errors} />
      <Button variant="secondary" busy={busy && !files} onClick={() => void list()}>List the backups</Button>
      {files?.length === 0 && <p className="mt-4 text-content-muted">The target holds no backup.</p>}
      {files && files.length > 0 && (
        <fieldset className="mt-5">
          <legend className="mb-1.5 text-sm font-medium">Backups on the target, newest first</legend>
          {files.map((file) => (
            <ChoiceField key={file.name} type="radio" name="backup" label={file.name} checked={chosen === file.name}
              description={`${new Date(file.modified).toLocaleString()} · ${formatBytes(file.sizeBytes)}`} onChange={() => setChosen(file.name)} />
          ))}
          <Button busy={busy} disabled={!chosen} onClick={() => chosen && void fetchBackup(chosen)}>Fetch the backup</Button>
        </fieldset>
      )}
    </>
  );
}
