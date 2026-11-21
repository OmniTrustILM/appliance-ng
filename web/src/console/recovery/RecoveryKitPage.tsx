import { useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, failureMessage, unreachable } from '../../setup/failure';
import { useLoaded } from '../shared/useLoaded';
import { IssueForm, ReissueForm, ReplaceForm } from './KitForms';

type Kit = components['schemas']['RecoveryKit'];

const loadKit = () => api.GET('/recovery-kit');

const superseded =
  'A newer kit was issued since this one, so this one is not confirmed. Download the latest kit by issuing it ' +
  'again or replacing it below, then confirm that one.';

/**
 * The latest kit: when it was issued, until when its talosconfig works, and its confirmation. Confirming names the
 * issuance of the kit downloaded on this page, or else of the kit the page loaded, so that a kit issued meanwhile
 * elsewhere is not confirmed in its place.
 */
function KitState({ kit, downloaded, onConfirmed }: Readonly<{ kit: Kit; downloaded?: string; onConfirmed: () => void }>) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [loaded] = useState(kit.issuance);

  async function confirm() {
    setBusy(true);
    setError(undefined);
    const issuance = downloaded ?? loaded ?? '';
    const result = await attempt(() => api.POST('/recovery-kit/confirm', { body: { issuance } }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) return onConfirmed();
    if (result.error?.code === 'kit_superseded') return setError(superseded);
    setError(result.error?.message ?? failureMessage(result.response.status));
  }

  return (
    <>
      {kit.issuedAt && <p className="m-0 text-sm">Issued on {new Date(kit.issuedAt).toLocaleString()}</p>}
      {kit.talosconfigNotAfter && (
        <p className="m-0 text-sm">
          Its talosconfig works until {new Date(kit.talosconfigNotAfter).toLocaleDateString()}; issue the kit again before then.
        </p>
      )}
      {kit.recipient && <p className="mt-1 break-all font-mono text-[13px] text-content-muted">Backup key {kit.recipient}</p>}
      <div className="mt-4">
        {error && <Alert tone="error">{error}</Alert>}
        {kit.confirmed ? (
          <Alert tone="success">The kit is confirmed.</Alert>
        ) : (
          <>
            <Alert tone="warning">
              The latest kit is not confirmed yet. Store it and its passphrase apart and safely, then confirm.
            </Alert>
            <Button busy={busy} onClick={() => void confirm()}>I have stored the kit safely</Button>
          </>
        )}
      </div>
    </>
  );
}

/** Issues, re-issues, replaces and confirms the recovery kit; the kit is downloaded and never kept. */
export function RecoveryKitPage() {
  const { data: kit, error, reload } = useLoaded(loadKit);
  const [issued, setIssued] = useState<string>();
  const [downloaded, setDownloaded] = useState<string>();

  function onIssued(filename: string, issuance: string | undefined) {
    setIssued(filename);
    setDownloaded(issuance);
    reload();
  }

  return (
    <PageWidth>
      <Card>
        <PageHeading eyebrow="Platform" title="Recovery kit"
          lead="The one file to keep: the backup key, break-glass access to the cluster and the node, and the appliance identity, encrypted with a passphrase. The appliance keeps no copy." />
        {issued && <Alert tone="success">{issued} was downloaded. Store it safely, then confirm it below.</Alert>}
        {error && <Alert tone="error">{error}</Alert>}
        {!kit && !error && <Loading />}
        {kit && !kit.issued && (
          <>
            <p className="mb-5 text-content-muted">
              No kit was issued yet. Choose a passphrase; without the kit and its passphrase, backups cannot be
              restored.
            </p>
            <IssueForm onIssued={onIssued} />
          </>
        )}
        {kit?.issued && <KitState kit={kit} downloaded={downloaded} onConfirmed={reload} />}
      </Card>

      {kit?.issued && (
        <>
          <div className="mt-6">
            <Card>
              <h2 className="mb-1.5 text-lg font-semibold">Issue the kit again</h2>
              <p className="mb-5 text-content-muted">
                With the current kit and its passphrase: the same backup key, a new passphrase and new break-glass
                access, before the talosconfig expires or when the passphrase must change. The earlier break-glass
                kubeconfig stops working.
              </p>
              <ReissueForm onIssued={onIssued} />
            </Card>
          </div>
          <div className="mt-6">
            <Card>
              <h2 className="mb-1.5 text-lg font-semibold">Replace a lost kit</h2>
              <p className="mb-5 text-content-muted">
                Only when the kit or its passphrase is lost: the new kit holds a new backup key.
              </p>
              <ReplaceForm onIssued={onIssued} />
            </Card>
          </div>
        </>
      )}
    </PageWidth>
  );
}
