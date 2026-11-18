import { useEffect, useState, type SubmitEvent } from 'react';
import { api, setCsrfToken } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { Field } from '../components/Field';
import { PageHeading } from '../components/PageHeading';
import { attempt, failureMessage, unreachable } from './failure';
import { normaliseCode } from './validate';

function seconds(count: number) {
  return `${count} ${count === 1 ? 'second' : 'seconds'}`;
}

/** Starts the setup session with the code shown on the appliance console. */
export function CodePage({ onDone }: Readonly<{ onDone: () => void }>) {
  const [code, setCode] = useState('');
  const [error, setError] = useState<string>();
  const [lockedFor, setLockedFor] = useState(0);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (lockedFor <= 0) return;
    const timer = setTimeout(() => setLockedFor(lockedFor - 1), 1000);
    return () => clearTimeout(timer);
  }, [lockedFor]);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    const result = await attempt(() => api.POST('/api/v1/setup/code', { body: { code } }));
    setBusy(false);
    if (!result) return setError(unreachable);
    const { data, error: body, response } = result;
    if (data) {
      setCsrfToken(data.csrfToken);
      return onDone();
    }
    if (response.status === 401) {
      setError('The setup code is not correct.');
      return setLockedFor(body && 'retryAfter' in body ? (body.retryAfter ?? 0) : 0);
    }
    if (response.status === 409) {
      return setError('The setup is in progress elsewhere: another browser has already started it.');
    }
    setError(failureMessage(response.status));
  }

  return (
    <form onSubmit={submit} noValidate>
      <Card actions={<Button type="submit" busy={busy} disabled={lockedFor > 0 || code.length === 0}>Continue</Button>}>
        <PageHeading eyebrow="Setup" title="Enter the setup code" lead="The code is shown on the console of the appliance." />
        <Field
          label="Setup code"
          name="code"
          variant="code"
          value={code}
          onChange={(event) => setCode(normaliseCode(event.target.value))}
          autoComplete="off"
          autoCapitalize="characters"
          spellCheck={false}
        />
        {error && <Alert tone="error">{error}</Alert>}
        {lockedFor > 0 && <Alert tone="warning">Too many attempts. Try again in {seconds(lockedFor)}.</Alert>}
      </Card>
    </form>
  );
}
