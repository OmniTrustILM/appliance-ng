import { useId, useState, type ChangeEvent } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { FileDropField } from '../../components/FileDropField';
import { INPUT } from '../../components/Field';
import { cx } from '../../components/cx';
import { attempt, unreachable } from '../../setup/failure';
import { CaDetails, type TrustedCertificate } from './CaDetails';

type Sent = { response: Response; error?: { message: string } };

const refusal = 'Nothing was added.';

function Preview({ items }: Readonly<{ items: TrustedCertificate[] }>) {
  const added = items.filter((ca) => ca.new);
  const present = items.filter((ca) => !ca.new);
  return (
    <div className="mb-[18px]">
      <h3 className="mb-1.5 font-semibold">New certificates ({added.length})</h3>
      {added.length === 0 && <p className="text-content-muted">Every certificate in the upload is already trusted.</p>}
      <ul className="m-0 list-none p-0">
        {added.map((ca) => (
          <li key={ca.sha256} className="border-b border-divider py-2.5 text-sm [overflow-wrap:anywhere]"><CaDetails ca={ca} /></li>
        ))}
      </ul>
      {present.length > 0 && (
        <p className="mt-3 text-sm text-content-muted">
          {present.length} already trusted and not added again.
        </p>
      )}
    </div>
  );
}

/** Adds CA certificates from a PEM bundle: the upload is previewed first and only a previewed bundle can be added. */
export function AddCaForm({ onAdded }: Readonly<{ onAdded: () => void }>) {
  const textId = useId();
  const [pem, setPem] = useState('');
  const [file, setFile] = useState<File>();
  const [preview, setPreview] = useState<TrustedCertificate[]>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  function change(text: string) {
    setPem(text);
    setPreview(undefined);
    setError(undefined);
  }

  async function pick(picked: File | undefined) {
    setFile(picked);
    if (!picked) return;
    const text = await attempt(() => picked.text());
    change(text ?? '');
    if (text === undefined) setError('The file could not be read.');
  }

  /** Runs a request; a failure is shown with the note that nothing was added and gives undefined. */
  async function run<R extends Sent>(call: () => Promise<R>): Promise<R | undefined> {
    setBusy(true);
    setError(undefined);
    const result = await attempt(call);
    setBusy(false);
    if (!result) {
      setError(`${unreachable} ${refusal}`);
      return undefined;
    }
    if (!result.response.ok) {
      setError(`${result.error?.message ?? 'The appliance could not process the request.'} ${refusal}`);
      return undefined;
    }
    return result;
  }

  async function previewUpload() {
    const result = await run(() => api.POST('/api/v1/trust/preview', { body: { pem } }));
    if (result?.data) setPreview(result.data.items);
  }

  async function addUpload() {
    if (await run(() => api.POST('/api/v1/trust', { body: { pem } }))) {
      setPem('');
      setFile(undefined);
      setPreview(undefined);
      onAdded();
    }
  }

  return (
    <form onSubmit={(event) => { event.preventDefault(); void previewUpload(); }} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <FileDropField label="Certificate file" required={false} file={file} onChange={(picked) => void pick(picked)}
        hint="A file with one or more CA certificates in PEM." />
      <div className="mb-[18px]">
        <label htmlFor={textId} className="mb-1.5 block text-sm font-medium">Or paste the certificates</label>
        <textarea
          id={textId}
          rows={6}
          spellCheck={false}
          value={pem}
          onChange={(event: ChangeEvent<HTMLTextAreaElement>) => change(event.target.value)}
          className={cx(INPUT, 'h-auto py-2 font-mono text-[13px]')}
        />
      </div>
      {preview && <Preview items={preview} />}
      <div className="flex flex-col gap-2 sm:flex-row">
        <Button type="submit" variant={preview ? 'secondary' : 'primary'} busy={busy} disabled={!pem.trim()}>Preview</Button>
        {preview?.some((ca) => ca.new) && (
          <Button busy={busy} onClick={() => void addUpload()}>Add the new certificates</Button>
        )}
      </div>
    </form>
  );
}
