import { useState } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Field } from '../../components/Field';
import { attempt, unreachable } from '../../setup/failure';

const stageMessages: Record<string, string> = {
  connect: 'The mail server cannot be reached. Check the server name, the port and the network.',
  tls: 'TLS failed. Check the security setting and that the server certificate is trusted.',
  auth: 'The mail server refused the user name or password.',
  send: 'The mail server refused the message. Check the from address and the recipient.',
};

type Result = { tone: 'success' | 'error'; message: string; settings: unknown };

/**
 * Sends a test mail through the saved mail server and reports how far it got. `settings` identifies the saved
 * settings; a result is shown only for the settings it was obtained with and while the form holds them.
 */
export function MailTest({ disabled, settings }: Readonly<{ disabled: boolean; settings: unknown }>) {
  const [to, setTo] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<Result>();

  async function send() {
    setBusy(true);
    setResult(undefined);
    const sent = await attempt(() => api.POST('/mail/test', { body: { to: to.trim() } }));
    setBusy(false);
    if (!sent) return setResult({ tone: 'error', message: unreachable, settings });
    if (sent.response.ok) return setResult({ tone: 'success', message: `The test mail was sent to ${to.trim()}.`, settings });
    const failure = sent.error;
    const stage = failure && 'stage' in failure ? stageMessages[failure.stage] : undefined;
    setResult({ tone: 'error', message: stage ?? failure?.message ?? 'The appliance could not process the request.', settings });
  }

  const shown = !disabled && result?.settings === settings ? result : undefined;

  return (
    <>
      <h2 className="mb-1.5 text-lg font-semibold">Send a test mail</h2>
      <p className="mb-5 text-content-muted">
        The test uses the saved mail settings{disabled ? '. Save the settings first' : ''}.
      </p>
      {shown && <Alert tone={shown.tone}>{shown.message}</Alert>}
      <Field label="Send the test mail to" name="to" type="email" value={to} onChange={(e) => setTo(e.target.value)} autoComplete="off" />
      <Button variant="secondary" busy={busy} disabled={disabled || !to.trim()} onClick={() => void send()}>Send a test mail</Button>
    </>
  );
}
