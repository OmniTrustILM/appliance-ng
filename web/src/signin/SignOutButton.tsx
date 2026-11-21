import { useState } from 'react';
import { useNavigate } from 'react-router';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
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
  const [busy, setBusy] = useState(false);

  async function click() {
    setError(undefined);
    setBusy(true);
    const result = await attempt(() => api.DELETE('/session'));
    setBusy(false);
    if (!result) return setError(unreachable);
    const { status, ok } = result.response;
    if (!ok && status !== 401) return setError(failureMessage(status));
    signOut();
    void navigate('/signin');
  }

  return (
    <>
      <Button variant="on-brand" busy={busy} onClick={click}>Sign out</Button>
      {error && (
        <div className="absolute inset-x-0 top-full z-10 px-4 pt-3 sm:px-6">
          <Alert tone="error">{error}</Alert>
        </div>
      )}
    </>
  );
}
