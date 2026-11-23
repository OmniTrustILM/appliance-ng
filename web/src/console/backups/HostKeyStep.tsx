import { useState } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { attempt, unreachable } from '../../setup/failure';

type HostKeyStepProps = Readonly<{
  host: string;
  port: number;
  accepted: string;
  error?: string;
  onAccept: (fingerprint: string) => void;
}>;

/**
 * Reads the SSH server's host key and lets the administrator accept it: nothing is saved with a key that was not
 * compared and accepted here.
 */
export function HostKeyStep({ host, port, accepted, error, onAccept }: HostKeyStepProps) {
  const [read, setRead] = useState<{ host: string; port: number; fingerprint: string }>();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  // A key read from another host or port is not offered.
  const scanned = read?.host === host.trim() && read.port === port ? read.fingerprint : undefined;

  async function scan() {
    setBusy(true);
    setFailure(undefined);
    setRead(undefined);
    const body = { host: host.trim(), port };
    const result = await attempt(() => api.POST('/backups/targets/hostkey', { body }));
    setBusy(false);
    if (!result) return setFailure(unreachable);
    if (result.data) return setRead({ ...body, fingerprint: result.data.fingerprint });
    setFailure(result.error?.message ?? 'The host key could not be read.');
  }

  function accept(fingerprint: string) {
    setRead(undefined);
    onAccept(fingerprint);
  }

  return (
    <div className="mb-[18px]">
      <p className="mb-1.5 text-sm font-medium">Host key</p>
      {accepted
        ? <p className="mb-2 text-sm">Accepted: <span className="break-all font-mono text-[13px]">{accepted}</span></p>
        : <p className="mb-2 text-sm text-content-muted">Read the server's host key and accept it before saving.</p>}
      {error && <p className="mb-2 text-[13px] text-danger" role="alert">{error}</p>}
      {failure && <Alert tone="error">{failure}</Alert>}
      {scanned && (
        <Alert tone="info">
          <span className="block">The server presents the host key <span className="break-all font-mono">{scanned}</span>.</span>
          <span className="block">Compare it with the fingerprint the server's administrator gives you before you accept it.</span>
          <span className="mt-2 block">
            <Button variant="secondary" onClick={() => accept(scanned)}>Accept this host key</Button>
          </span>
        </Alert>
      )}
      <Button variant="secondary" busy={busy} disabled={!host.trim()} onClick={() => void scan()}>
        {accepted ? 'Read the host key again' : 'Read the host key'}
      </Button>
    </div>
  );
}
