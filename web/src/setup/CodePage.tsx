import { useEffect, useState, type SubmitEvent } from 'react';
import { api, setCsrfToken } from '../api/client';
import { Field } from '../components/Field';
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
      <h1>Enter the setup code</h1>
      <p>The code is shown on the console of the appliance.</p>
      <Field
        label="Setup code"
        name="code"
        value={code}
        onChange={(event) => setCode(normaliseCode(event.target.value))}
        autoComplete="off"
        autoCapitalize="characters"
        spellCheck={false}
      />
      {error && <p className="form-error" role="alert">{error}</p>}
      {lockedFor > 0 && <p>Too many attempts. Try again in {seconds(lockedFor)}.</p>}
      <button type="submit" disabled={busy || lockedFor > 0 || code.length === 0}>Continue</button>
    </form>
  );
}
