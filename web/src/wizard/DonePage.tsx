import { useEffect, useState, type SubmitEvent } from 'react';
import { api } from '../api/client';
import { Field } from '../components/Field';
import { SignOutButton } from '../signin/SignOutButton';
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
    void attempt(() => api.GET('/api/v1/appliance')).then((result) => {
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
    const result = await attempt(() => api.POST('/api/v1/platform/admin-credential', { body: { password }, parseAs: 'blob' }));
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
    <div>
      <h1>The ILM is ready</h1>
      <SignOutButton />
      {address && <p>Open the ILM at <a href={address}>{address}</a></p>}
      <p><RootCaLink /> and import it where clients should trust the ILM.</p>
      <h2>Administrator credential</h2>
      {downloaded ? (
        <p>The administrator credential was downloaded as {fileName}. It cannot be downloaded again, so keep the file and its password safe.</p>
      ) : (
        <form onSubmit={download} noValidate>
          <p>
            Sign in to the ILM with a client certificate. It is handed out once, as a PKCS#12 file that this password
            protects. The appliance does not keep the password.
          </p>
          <Field label="Certificate password" name="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} error={fieldError} autoComplete="new-password" />
          <button type="submit" disabled={busy}>Download</button>
        </form>
      )}
      {message && <p className="form-error" role="alert">{message}</p>}
    </div>
  );
}
