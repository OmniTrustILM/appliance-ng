import { useState } from 'react';
import { useNavigate } from 'react-router';
import { api } from '../api/client';
import { useSessionControl } from '../session';
import { attempt, failureMessage, unreachable } from '../setup/failure';

/**
 * Ends the console session. The user is signed out here once the appliance ended the session, or had ended it
 * already; when it could not, the session stays and the user can try again.
 */
export function SignOutButton() {
  const navigate = useNavigate();
  const { signOut } = useSessionControl();
  const [error, setError] = useState<string>();

  async function click() {
    setError(undefined);
    const result = await attempt(() => api.DELETE('/api/v1/session'));
    if (!result) return setError(unreachable);
    const { status, ok } = result.response;
    if (!ok && status !== 401) return setError(failureMessage(status));
    signOut();
    void navigate('/signin');
  }

  return (
    <>
      <button type="button" onClick={click}>Sign out</button>
      {error && <p className="form-error" role="alert">{error}</p>}
    </>
  );
}
