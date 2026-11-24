import { useState } from 'react';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { ChoiceField } from '../components/ChoiceField';
import { FileDropField } from '../components/FileDropField';
import { attempt } from '../setup/failure';
import { refusalOf, uploadBackup } from './restoreApi';
import { TargetSource } from './TargetSource';
import { useLatest } from './useLatest';

type OnReady = Readonly<{ onReady: (name: string) => void }>;

/** Uploads a backup file from this computer. */
function UploadSource({ onReady }: OnReady) {
  const [file, setFile] = useState<File>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const begin = useLatest();

  /** An upload that finishes after another source was chosen is dropped. */
  async function upload() {
    if (!file) return;
    const latest = begin();
    setBusy(true);
    setError(undefined);
    const refusal = refusalOf(await attempt(() => uploadBackup(file)));
    if (!latest()) return;
    setBusy(false);
    if (refusal) return setError(refusal.message);
    onReady(file.name);
  }

  return (
    <>
      {error && <Alert tone="error">{error}</Alert>}
      <FileDropField label="Backup file" required file={file} onChange={setFile} hint="An .ilmbackup file downloaded from an appliance or copied from a target." />
      <Button busy={busy} disabled={!file} onClick={() => void upload()}>Upload the backup</Button>
    </>
  );
}

/** Where the backup to restore comes from: an upload, or a target entered here with a file from its listing. */
export function SourceStep({ onReady }: OnReady) {
  const [kind, setKind] = useState<'upload' | 'target'>('upload');
  return (
    <>
      <fieldset className="mb-[18px]">
        <legend className="mb-1.5 text-sm font-medium">The backup</legend>
        <ChoiceField type="radio" name="source" label="Upload a backup file" checked={kind === 'upload'} onChange={() => setKind('upload')} />
        <ChoiceField type="radio" name="source" label="Fetch it from a target" checked={kind === 'target'} onChange={() => setKind('target')} />
      </fieldset>
      {kind === 'upload' ? <UploadSource onReady={onReady} /> : <TargetSource onReady={onReady} />}
    </>
  );
}
