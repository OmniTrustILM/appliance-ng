import { useState, type SubmitEvent } from 'react';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { attempt, unreachable } from '../../setup/failure';
import { FileDropField } from '../../components/FileDropField';

type Sent = { response: Response; error?: { message: string } };

type UploadFormProps = Readonly<{
  withKey: boolean;
  submitLabel: string;
  send: (form: FormData) => Promise<Sent>;
  onStored: () => void;
}>;

const refusal = 'The current certificate stays in use.';
const unconfirmed = 'It is not known whether the certificate was stored. Reload the page to see the certificate in use.';

/** Uploads a certificate, with its private key or alone, and a chain; a refusal names the problem. */
export function UploadForm({ withKey, submitLabel, send, onStored }: UploadFormProps) {
  const [certificate, setCertificate] = useState<File>();
  const [key, setKey] = useState<File>();
  const [chain, setChain] = useState<File>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!certificate || (withKey && !key)) return;
    const form = new FormData();
    form.append('certificate', certificate);
    if (withKey && key) form.append('key', key);
    if (chain) form.append('chain', chain);
    setBusy(true);
    setError(undefined);
    const result = await attempt(() => send(form));
    setBusy(false);
    if (result?.response.ok) return onStored();
    // Without an answer, or with a server error, the upload may have been stored all the same.
    const reason = result ? (result.error?.message ?? 'The appliance could not process the request.') : unreachable;
    const uncertain = !result || result.response.status >= 500;
    setError(`${reason} ${uncertain ? unconfirmed : refusal}`);
  }

  return (
    <form onSubmit={(event) => void submit(event)} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <FileDropField label="Certificate" required file={certificate} onChange={setCertificate}
        hint="The certificate in PEM, optionally followed by its chain." />
      {withKey && (
        <FileDropField label="Private key" required file={key} onChange={setKey}
          hint="The key of the certificate in PEM, not encrypted." />
      )}
      <FileDropField label="Chain" file={chain} onChange={setChain}
        hint="The intermediate and root certificates in PEM, when they are not in the certificate file." />
      <Button type="submit" busy={busy} disabled={!certificate || (withKey && !key)}>{submitLabel}</Button>
    </form>
  );
}
