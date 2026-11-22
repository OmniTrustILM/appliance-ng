import { useEffect, useState, type SubmitEvent } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button, buttonClass } from '../components/Button';
import { cx } from '../components/cx';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';
import { PageWidth } from '../components/PageWidth';
import { Field } from '../components/Field';
import { attempt, failureMessage, unreachable } from '../setup/failure';
import { RootCaLink } from './RootCaLink';

const fileName = 'ilm-administrator.p12';
/** How long the file's URL outlives the click; some browsers start the download only after the click returns. */
const revokeDelay = 1000;

function save(file: Blob) {
  const url = URL.createObjectURL(file);
  const link = document.createElement('a');
  link.href = url;
  link.download = fileName;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), revokeDelay);
}

/** The end of the setup: where the ILM is, the root certificate, and the one-time administrator credential. */
export function DonePage() {
  const [hostName, setHostName] = useState<string>();
  const [password, setPassword] = useState('');
  const [fieldError, setFieldError] = useState<string>();
  const [message, setMessage] = useState<string>();
  const [downloaded, setDownloaded] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let active = true;
    void attempt(() => api.GET('/appliance')).then((result) => {
      if (!active) return;
      if (result?.data) setHostName(result.data.platform?.hostName);
      else setMessage(unreachable);
    });
    return () => {
      active = false;
    };
  }, []);

  async function download(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setFieldError(undefined);
    setMessage(undefined);
    const result = await attempt(() => api.POST('/platform/admin-credential', { body: { password }, parseAs: 'blob' }));
    setBusy(false);
    if (!result) return setMessage(unreachable);
    const { response, data, error } = result;
    if (response.ok && data) {
      save(data);
      setPassword('');
      return setDownloaded(true);
    }
    if (response.status === 400 && error && 'field' in error && error.field === 'password') return setFieldError(error.message);
    if (response.status === 410) return setMessage('The administrator credential was already downloaded and cannot be downloaded again.');
    if (response.status === 404) return setMessage('The administrator credential has not been issued yet. Try again in a few minutes.');
    setMessage(failureMessage(response.status));
  }

  const address = hostName && `https://${hostName}/`;
  return (
    <PageWidth>
      <Card>
        <div className="pt-2 text-center">
          <span aria-hidden="true" className="mb-3 inline-grid size-14 place-items-center rounded-full bg-success-surface text-[28px] font-bold text-success">✓</span>
        </div>
        <div className="pb-4">
          <PageHeading title="The ILM is ready" centered />
        </div>
        {address && (
          <div className="mb-4 flex flex-wrap items-center justify-between gap-2 rounded-lg border border-divider bg-surface-sunken px-4 py-3.5">
            <span>Open the ILM at</span>
            <a href={address} className={cx(buttonClass('primary'), 'h-auto min-h-10 max-w-full whitespace-normal! break-all py-2 font-mono')}>{address}</a>
          </div>
        )}
        <p><RootCaLink /> and import it where clients should trust the ILM.</p>
        <h2 className="mb-3 mt-6 border-t border-divider pt-5 text-sm font-semibold">Administrator credential</h2>
        {downloaded ? (
          <p>The administrator credential was downloaded as {fileName}. It cannot be downloaded again, so keep the file and its password safe.</p>
        ) : (
          <form onSubmit={download} noValidate>
            <p className="mb-3.5 text-content-muted">
              Sign in to the ILM with a client certificate. It is handed out once, as a PKCS#12 file that this password
              protects. The appliance does not keep the password.
            </p>
            <Field label="Certificate password" name="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} error={fieldError} autoComplete="new-password" />
            <Button type="submit" variant="secondary" busy={busy}>Download</Button>
          </form>
        )}
        {message && <div className="mt-4"><Alert tone="error">{message}</Alert></div>}
      </Card>
    </PageWidth>
  );
}
