import { useState } from 'react';
import { api } from '../../api/client';
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
import { RegistryForm, type Registry } from './RegistryForm';

const saved = 'The change is saved. The platform and the node pick it up within a minute; no restart is needed.';

const loadRegistries = () => api.GET('/api/v1/registries');
const removeRegistry = (registry: Registry) =>
  api.DELETE('/api/v1/registries/{host}', { params: { path: { host: registry.host } } });

/** Lists the registries with credentials, adds or changes them and removes them; a password is never shown. */
export function RegistriesPage() {
  const { items, error, reload } = useList<Registry>(loadRegistries);
  const [editing, setEditing] = useState<Registry>();
  const [notice, setNotice] = useState(false);
  const [saves, setSaves] = useState(0);
  const removal = useRemoval(removeRegistry, removed);

  function changed() {
    setNotice(true);
    reload();
  }

  function removed(registry: Registry) {
    if (editing?.host === registry.host) setEditing(undefined);
    changed();
  }

  function formSaved() {
    setEditing(undefined);
    setSaves((count) => count + 1);
    changed();
  }

  return (
    <PageWidth>
      <Card>
        <PageHeading eyebrow="Platform" title="Registries" lead="The container registries the platform and the node pull images from with credentials." />
        {notice && <Alert tone="success">{saved}</Alert>}
        {error && <Alert tone="error">{error}</Alert>}
        {removal.error && <Alert tone="error">{removal.error}</Alert>}
        {!items && !error && <Loading />}
        {items?.length === 0 && <p className="text-content-muted">No registries have credentials.</p>}
        {items && items.length > 0 && (
          <ul className="m-0 list-none p-0">
            {items.map((registry) => (
              <ItemRow
                key={registry.host}
                actions={(
                  <>
                    <Button variant="secondary" aria-label={`Edit ${registry.host}`} onClick={() => setEditing(registry)}>Edit</Button>
                    <Button variant="secondary" aria-label={`Remove ${registry.host}`} onClick={() => removal.ask(registry)}>Remove</Button>
                  </>
                )}
              >
                <p className="m-0 font-medium">{registry.host}</p>
                <p className="m-0 text-content-muted">User name: {registry.username || 'none'}</p>
                <p className="m-0 mt-1">
                  <Badge tone={registry.passwordSet ? 'ok' : 'muted'}>{registry.passwordSet ? 'Password set' : 'No password'}</Badge>
                </p>
              </ItemRow>
            ))}
          </ul>
        )}
      </Card>

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">{editing ? `Edit ${editing.host}` : 'Add a registry'}</h2>
          <p className="mb-5 text-content-muted">
            {editing
              ? 'Change the user name or set a new password. The password cannot be shown.'
              : 'Store the credentials the platform and the node use to pull images from a registry. Saving the credentials of a registry that is listed replaces them.'}
          </p>
          <RegistryForm
            key={editing?.host ?? `new-${saves}`}
            editing={editing}
            onSaved={formSaved}
            onCancel={() => setEditing(undefined)}
          />
        </Card>
      </div>

      {removal.asking && (
        <ConfirmDialog
          title="Remove this registry?"
          confirmLabel="Remove"
          onConfirm={removal.confirm}
          onCancel={removal.cancel}
        >
          The credentials for {removal.asking.host} are deleted. Pulls from it work only if it allows anonymous access.
          A chosen component that pulls from it needs the credentials; remove it from the components first.
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
