import { useEffect, useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router';
import { api } from '../api/client';
import { Field } from '../components/Field';
import { useSessionControl } from '../session';
import { attempt, failureMessage, unreachable } from '../setup/failure';

/** One message for every failed sign-in, so that it never tells which of the three was wrong. */
const rejected = 'The user name, password or code is not correct.';

type Failure = { message: string; serverTime?: string };

/** Signs in with the user name, the password and the authenticator code. */
export function SignInPage() {
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [totp, setTotp] = useState('');
  const [failure, setFailure] = useState<Failure>();
  const [remaining, setRemaining] = useState<number>();
  const [busy, setBusy] = useState(false);
  const { signIn } = useSessionControl();
  const navigate = useNavigate();

  // Counts the lock-out down; the effect restarts with each new value and stops when the page closes.
  useEffect(() => {
    if (remaining === undefined) return;
    const timer = setTimeout(() => setRemaining(remaining > 1 ? remaining - 1 : undefined), 1000);
    return () => clearTimeout(timer);
  }, [remaining]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setFailure(undefined);
    const result = await attempt(() => api.POST('/api/v1/session', { body: { user, password, totp } }));
    setBusy(false);
    if (!result) return setFailure({ message: unreachable });
    const { response, data, error } = result;
    if (response.ok && data) {
      signIn(user, data.csrfToken);
      const status = await attempt(() => api.GET('/api/v1/status'));
      return navigate(status?.data?.setup === 'part2' || status?.data?.setup === 'part1' ? '/wizard/' : '/');
    }
    setPassword('');
    setTotp('');
    if (response.status === 401 && error && 'retryAfter' in error) return setRemaining(error.retryAfter);
    if (response.status === 400 && error && 'serverTime' in error) {
      return setFailure({ message: rejected, serverTime: error.serverTime });
    }
    setFailure({ message: failureMessage(response.status) });
  }

  return (
    <form onSubmit={submit} noValidate>
      <h1>Sign in</h1>
      <Field label="User name" name="user" value={user} onChange={(e) => setUser(e.target.value)} autoComplete="username" />
      <Field label="Password" name="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
      <Field label="Authenticator code" name="totp" value={totp} onChange={(e) => setTotp(e.target.value)} autoComplete="one-time-code" inputMode="numeric" />
      {failure && (
        <p className="form-error" role="alert">
          {failure.message}
          {failure.serverTime && <> The appliance time is {failure.serverTime}; a code needs a correct clock.</>}
        </p>
      )}
      {remaining !== undefined && (
        <p className="form-error" role="alert">Too many failed attempts. Try again in {remaining} seconds.</p>
      )}
      <button type="submit" disabled={busy || remaining !== undefined}>Sign in</button>
    </form>
  );
}
