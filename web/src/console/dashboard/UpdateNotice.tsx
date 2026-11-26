import { useEffect, useState } from 'react';
import { Link } from 'react-router';
import { api, timeoutSignal } from '../../api/client';
import { Alert } from '../../components/Alert';
import { attempt } from '../../setup/failure';

const LINK = 'text-brand hover:underline';

/** The kinds of the available bundles that can be installed, read once; empty while unread or when the read fails. */
function useInstallableKinds() {
  const [kinds, setKinds] = useState<string[]>([]);
  useEffect(() => {
    let active = true;
    void attempt(() => api.GET('/updates', { signal: timeoutSignal() })).then((result) => {
      if (!active || !result?.data) return;
      setKinds([...new Set(result.data.available.filter((b) => !b.blocked).map((b) => b.kind))]);
    });
    return () => {
      active = false;
    };
  }, []);
  return kinds;
}

/** The notice that a newer system or platform bundle is available; nothing when there is none. */
export function UpdateNotice() {
  const kinds = useInstallableKinds();
  if (kinds.length === 0) return null;
  return (
    <Alert tone="info">
      A newer {kinds.join(' and ')} update is available. <Link className={LINK} to="/updates">Open the updates page</Link>
    </Alert>
  );
}
