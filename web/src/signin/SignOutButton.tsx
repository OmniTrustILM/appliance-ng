import { useNavigate } from 'react-router';
import { api } from '../api/client';
import { useSessionControl } from '../session';
import { attempt } from '../setup/failure';

/** Ends the console session. The user is signed out here even when the appliance had ended it already. */
export function SignOutButton() {
  const navigate = useNavigate();
  const { signOut } = useSessionControl();

  async function click() {
    await attempt(() => api.DELETE('/api/v1/session'));
    signOut();
    void navigate('/signin');
  }

  return <button type="button" onClick={click}>Sign out</button>;
}
