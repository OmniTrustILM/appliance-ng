import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { Field } from '../components/Field';
import { PageHeading } from '../components/PageHeading';
import { attempt, failureMessage, unreachable } from './failure';
import { QrCode } from './QrCode';

type Enrolment = { seed: string; otpauthUrl: string };

function utc(time: string) {
  return time.replace('T', ' ').replace('Z', ' UTC');
}

/** Shows the authenticator secret once and checks the first code. The secret lives in this component only. */
export function TotpPage({ onDone, onExpired }: Readonly<{ onDone: () => void; onExpired: () => void }>) {
  const [enrolment, setEnrolment] = useState<Enrolment>();
  const [code, setCode] = useState('');
  const [fieldError, setFieldError] = useState<string>();
  const [error, setError] = useState<string>();
  const [serverTime, setServerTime] = useState<string>();
  const [busy, setBusy] = useState(false);

  // Every enrolment discards the seed of the one before, so a mount asks for one only, also when StrictMode runs the
  // effect twice.
  const requested = useRef(false);
  useEffect(() => {
    if (requested.current) return;
    requested.current = true;
    void attempt(() => api.POST('/api/v1/setup/totp')).then((result) => {
      if (!result) return setError(unreachable);
      if (result.data) return setEnrolment(result.data);
      if (result.response.status === 401) return onExpired();
      setError(failureMessage(result.response.status));
    });
  }, [onExpired]);

  async function submit(event: SubmitEvent) {
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
      <Card actions={<Button type="submit" busy={busy} disabled={!enrolment}>Continue</Button>}>
        <PageHeading
          eyebrow="Setup"
          title="Authenticator"
          lead="Scan the QR code with an authenticator app, or enter the secret by hand. It is shown only now."
        />
        <div className={enrolment ? 'grid items-start gap-6 sm:grid-cols-[200px_1fr]' : undefined}>
          {enrolment && <QrCode value={enrolment.otpauthUrl} label="QR code of the authenticator secret" />}
          <div className="min-w-0">
            <ol className="mb-4 list-decimal space-y-1 pl-5 text-sm text-content-muted">
              <li>Open your authenticator app and add an account.</li>
              <li>Scan the code, or type the secret below.</li>
              <li>Enter the six-digit code the app shows.</li>
            </ol>
            {enrolment && (
              <>
                <p className="mb-1.5 text-sm font-medium">Secret</p>
                <p className="mb-[18px] select-all break-all rounded-md bg-surface-sunken px-3 py-2.5 font-mono text-sm">{enrolment.seed}</p>
              </>
            )}
            <Field
              label="Code from the authenticator"
              name="code"
              variant="code"
              value={code}
              onChange={(e) => setCode(e.target.value.trim())}
              error={fieldError}
              hint={serverTime && `Appliance time: ${utc(serverTime)}. Your device time: ${utc(new Date().toISOString())}.`}
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={6}
            />
          </div>
        </div>
        {error && <Alert tone="error">{error}</Alert>}
      </Card>
    </form>
  );
}
