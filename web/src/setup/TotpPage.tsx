import { useEffect, useState, type FormEvent } from 'react';
import { api } from '../api/client';
import { Field } from '../components/Field';
import { attempt, failureMessage, unreachable } from './failure';
import { QrCode } from './QrCode';

type Enrolment = { seed: string; otpauthUrl: string };

function utc(time: string) {
  return time.replace('T', ' ').replace('Z', ' UTC');
}

/** Shows the authenticator secret once and checks the first code. The secret lives in this component only. */
export function TotpPage({ onDone, onExpired }: { onDone: () => void; onExpired: () => void }) {
  const [enrolment, setEnrolment] = useState<Enrolment>();
  const [code, setCode] = useState('');
  const [fieldError, setFieldError] = useState<string>();
  const [error, setError] = useState<string>();
  const [serverTime, setServerTime] = useState<string>();
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let active = true;
    void attempt(() => api.POST('/api/v1/setup/totp')).then((result) => {
      if (!active) return;
      if (!result) return setError(unreachable);
      if (result.data) return setEnrolment(result.data);
      if (result.response.status === 401) return onExpired();
      setError(failureMessage(result.response.status));
    });
    return () => {
      active = false;
    };
  }, [onExpired]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError(undefined);
    setServerTime(undefined);
    if (!/^\d{6}$/.test(code)) return setFieldError('Enter the 6 digits shown by the authenticator.');
    setFieldError(undefined);
    setBusy(true);
    const result = await attempt(() => api.POST('/api/v1/setup/totp/confirm', { body: { code } }));
    setBusy(false);
    if (!result) return setError(unreachable);
    const { response, error: body } = result;
    if (response.ok) return onDone();
    if (response.status === 401) return onExpired();
    if (response.status === 400) {
      setServerTime(body && 'serverTime' in body ? body.serverTime : undefined);
      return setError('The code was not accepted. Check the code and the clock of the authenticator.');
    }
    setError(failureMessage(response.status));
  }

  return (
    <form onSubmit={submit} noValidate>
      <h1>Authenticator</h1>
      {enrolment && (
        <>
          <p>Scan the QR code with an authenticator app, or enter the secret by hand. It is shown only now.</p>
          <QrCode value={enrolment.otpauthUrl} label="QR code of the authenticator secret" />
          <p>Secret: <code>{enrolment.seed}</code></p>
        </>
      )}
      <Field label="Code from the authenticator" name="code" value={code} onChange={(e) => setCode(e.target.value.trim())} error={fieldError} inputMode="numeric" autoComplete="one-time-code" maxLength={6} />
      {error && <p className="form-error" role="alert">{error}</p>}
      {serverTime && <p>Appliance time: {utc(serverTime)}. Your device time: {utc(new Date().toISOString())}.</p>}
      <button type="submit" disabled={busy || !enrolment}>Continue</button>
    </form>
  );
}
