import { useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { ItemRow } from '../shared/ItemRow';
import { useList } from '../shared/useList';
import { useRemoval } from '../shared/useRemoval';
import { KubeconfigForm } from './KubeconfigForm';
import { TalosconfigForm } from './TalosconfigForm';

type Credential = components['schemas']['AccessCredential'];

const loadAccess = () => api.GET('/access');
const revokeKubeconfig = (credential: Credential) =>
  api.DELETE('/access/kubeconfig/{name}', { params: { path: { name: credential.name ?? '' } } });

function CredentialDetails({ credential }: Readonly<{ credential: Credential }>) {
  const title = credential.kind === 'kubeconfig' ? credential.name : 'talosconfig';
  return (
    <>
      <p className="m-0 font-medium">{title} <Badge tone="muted">{credential.role}</Badge></p>
      {credential.breakGlass && (
        <p className="m-0 text-content-muted">Break-glass access of the recovery kit; issuing the recovery kit again revokes it.</p>
      )}
      <p className="m-0 text-content-muted">
        {credential.expires ? `Expires ${new Date(credential.expires).toLocaleString()}` : 'Valid until revoked'}
      </p>
      <p className="m-0 text-content-muted">
        Issued by {credential.issuedBy || 'unknown'} on {new Date(credential.issuedAt).toLocaleString()}
      </p>
    </>
  );
}

/** Issues kubeconfigs and talosconfigs, lists them and revokes kubeconfigs; the issued files are never kept. */
export function AccessPage() {
  const { items, error, reload } = useList<Credential>(loadAccess);
  const [issued, setIssued] = useState<string>();
  const removal = useRemoval(revokeKubeconfig, reload);
  const kubeconfigs = items?.filter((c) => c.kind === 'kubeconfig');
  const talosconfigs = items?.filter((c) => c.kind === 'talosconfig');

  function onIssued(filename: string) {
    setIssued(filename);
    reload();
  }

  return (
    <PageWidth>
      <Card>
        <PageHeading eyebrow="Platform" title="Cluster access"
          lead="Kubeconfigs for the cluster and talosconfigs for the node, for administration and troubleshooting." />
        {issued && <Alert tone="success">{issued} was downloaded. The appliance does not keep a copy; store it safely.</Alert>}
        {error && <Alert tone="error">{error}</Alert>}
        {removal.error && <Alert tone="error">{removal.error}</Alert>}
        {!items && !error && <Loading />}
        <h2 className="mb-2 mt-2 text-sm font-semibold">Kubeconfigs</h2>
        {kubeconfigs?.length === 0 && <p className="text-content-muted">No kubeconfigs are issued.</p>}
        {kubeconfigs && kubeconfigs.length > 0 && (
          <ul className="m-0 list-none p-0">
            {kubeconfigs.map((credential) => (
              <ItemRow key={credential.name} actions={!credential.breakGlass && (
                <Button variant="secondary" aria-label={`Revoke ${credential.name}`} onClick={() => removal.ask(credential)}>Revoke</Button>
              )}>
                <CredentialDetails credential={credential} />
              </ItemRow>
            ))}
          </ul>
        )}
        <h2 className="mb-2 mt-6 text-sm font-semibold">Talosconfigs</h2>
        <p className="text-content-muted">Talosconfigs cannot be revoked; each stops working when it expires.</p>
        {talosconfigs?.length === 0 && <p className="text-content-muted">No talosconfigs are valid.</p>}
        {talosconfigs && talosconfigs.length > 0 && (
          <ul className="m-0 list-none p-0">
            {talosconfigs.map((credential, index) => (
              // Records carry no identity of their own and two can match to the second; the rows hold no state.
              <li key={`${credential.issuedAt}|${credential.role}|${credential.expires}|${index}`} className="border-b border-divider py-3 text-sm last:border-b-0">
                <CredentialDetails credential={credential} />
              </li>
            ))}
          </ul>
        )}
      </Card>

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">Issue a kubeconfig</h2>
          <p className="mb-5 text-content-muted">A kubeconfig for kubectl against the cluster on this appliance.</p>
          <KubeconfigForm onIssued={onIssued} />
        </Card>
      </div>

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">Issue a talosconfig</h2>
          <p className="mb-5 text-content-muted">
            A talosconfig for talosctl against the node. It cannot be revoked, so issue it for as short a time as the
            work needs.
          </p>
          <TalosconfigForm onIssued={onIssued} />
        </Card>
      </div>

      {removal.asking && (
        <ConfirmDialog title="Revoke this kubeconfig?" confirmLabel="Revoke" onConfirm={removal.confirm} onCancel={removal.cancel}>
          {removal.asking.name} stops working at once and cannot be restored; issue a new kubeconfig if it is needed again.
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
