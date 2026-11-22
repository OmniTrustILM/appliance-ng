import { useState } from 'react';
import { Link } from 'react-router';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { ItemRow } from '../shared/ItemRow';
import { useList } from '../shared/useList';
import { useRemoval } from '../shared/useRemoval';
import { AddCaForm } from './AddCaForm';
import { CaDetails, type TrustedCertificate } from './CaDetails';

const loadTrust = () => api.GET('/trust');
const removeTrust = (ca: TrustedCertificate) =>
  api.DELETE('/trust/{sha256}', { params: { path: { sha256: ca.sha256 } } });

/** Lists the CA certificates trusted in addition to the public ones, adds more after a preview and removes them. */
export function TrustPage() {
  const { items, error, reload } = useList<TrustedCertificate>(loadTrust);
  const [changed, setChanged] = useState(false);
  const removal = useRemoval(removeTrust, () => {
    setChanged(true);
    reload();
  });

  function added() {
    setChanged(true);
    reload();
  }

  return (
    <PageWidth>
      <Card>
        <PageHeading eyebrow="Platform" title="Trusted CAs" lead="The CA certificates the platform and the node trust in addition to the public ones, for example for a private image registry." />
        {changed && (
          <Alert tone="success">
            The change is saved. The node needs a restart before image pulls use it: <Link className="text-brand hover:underline" to="/system">restart the node</Link>.
          </Alert>
        )}
        {error && <Alert tone="error">{error}</Alert>}
        {removal.error && <Alert tone="error">{removal.error}</Alert>}
        {!items && !error && <Loading />}
        {items?.length === 0 && <p className="text-content-muted">No additional CA certificates are trusted.</p>}
        {items && items.length > 0 && (
          <ul className="m-0 list-none p-0">
            {items.map((ca) => (
              <ItemRow key={ca.sha256} actions={<Button variant="secondary" aria-label={`Remove ${ca.subject}`} onClick={() => removal.ask(ca)}>Remove</Button>}>
                <CaDetails ca={ca} />
              </ItemRow>
            ))}
          </ul>
        )}
      </Card>

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">Add CA certificates</h2>
          <p className="mb-5 text-content-muted">
            Choose or paste a PEM bundle and preview it first. Only CA certificates are accepted, and a certificate that is already trusted is not added again.
          </p>
          <AddCaForm onAdded={added} />
        </Card>
      </div>

      {removal.asking && (
        <ConfirmDialog
          title="Stop trusting this CA?"
          confirmLabel="Remove"
          onConfirm={removal.confirm}
          onCancel={removal.cancel}
        >
          {removal.asking.subject} is no longer trusted once the node restarts. Image pulls from registries that depend on it fail until it is added again.
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
