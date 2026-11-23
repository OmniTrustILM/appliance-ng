import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { useLoaded } from '../shared/useLoaded';
import { useRemoval } from '../shared/useRemoval';
import { LocalBackups } from './LocalBackups';
import { TargetForm, type Target } from './TargetForm';
import { TargetRow } from './TargetRow';

/** How often the page reads the backups again while one runs. */
export const runningRefreshMs = 5000;
/** How many reads after "Back up now" look for the accepted backup before the page stops looking. */
export const startedReads = 6;
const removeTarget = (target: Target) =>
  api.DELETE('/backups/targets/{name}', { params: { path: { name: target.name } } });

/** The local backups and the targets that receive copies on a schedule; target secrets are never shown. */
export function BackupsPage() {
  const background = useRef(false);
  // Marked as a background refresh, a read the page makes on its own does not keep the session from going idle.
  const loadBackups = useCallback(
    () => api.GET('/backups', background.current ? { params: { header: { 'X-Background-Refresh': '1' } } } : {}),
    [],
  );
  const { data, error, reload: load, settled } = useLoaded(loadBackups);
  const [editing, setEditing] = useState<Target>();
  const [saves, setSaves] = useState(0);
  // Reads left to find a backup that was accepted but does not show as running yet.
  const [awaited, setAwaited] = useState(0);
  const removal = useRemoval(removeTarget, removed);
  const running = data?.running === true;

  const reload = useCallback(() => {
    background.current = false;
    load();
  }, [load]);

  useEffect(() => {
    if (!running && awaited === 0) return undefined;
    const timer = setTimeout(() => {
      setAwaited((left) => (running ? 0 : left - 1));
      background.current = true;
      load();
    }, runningRefreshMs);
    return () => clearTimeout(timer);
  }, [running, awaited, settled, load]);

  function started() {
    setAwaited(startedReads);
    reload();
  }

  function removed(target: Target) {
    if (editing?.name === target.name) setEditing(undefined);
    reload();
  }

  function saved() {
    setEditing(undefined);
    setSaves((count) => count + 1);
    reload();
  }

  const unhealthy = data?.healthy?.status === 'False' ? data.healthy : undefined;

  return (
    <PageWidth>
      <Card>
        <PageHeading title="Backups" lead="Encrypted backups of the appliance and the ILM database, kept on the appliance and copied to targets on a schedule." />
        {error && <Alert tone="error">{error}</Alert>}
        {unhealthy && <Alert tone="warning">Backups are not healthy: {unhealthy.message ?? unhealthy.reason}</Alert>}
        {!data && !error && <Loading />}
        {data && <LocalBackups backups={data} onStarted={started} />}
      </Card>

      {data && (
        <div className="mt-6">
          <Card>
            <h2 className="mb-1.5 text-lg font-semibold">Targets</h2>
            {removal.error && <Alert tone="error">{removal.error}</Alert>}
            {data.targets.length === 0 && <p className="text-content-muted">No target receives copies yet.</p>}
            <ul className="m-0 list-none p-0">
              {data.targets.map((target) => (
                <TargetRow key={`${target.name}-${saves}`} target={target} onEdit={() => setEditing(target)} onRemove={() => removal.ask(target)} />
              ))}
            </ul>
          </Card>
        </div>
      )}

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">{editing ? `Edit ${editing.name}` : 'Add a target'}</h2>
          <p className="mb-5 text-content-muted">
            {editing
              ? 'Change the settings. Stored secrets cannot be shown; leave them blank to keep them.'
              : 'Each target receives a copy of the backup at its scheduled time and keeps the newest ones.'}
          </p>
          <TargetForm key={editing?.name ?? `new-${saves}`} editing={editing} onSaved={saved} onCancel={() => setEditing(undefined)} />
        </Card>
      </div>

      {removal.asking && (
        <ConfirmDialog title="Remove this target?" confirmLabel="Remove" onConfirm={removal.confirm} onCancel={removal.cancel}>
          {removal.asking.name} receives no more copies and its stored secrets are deleted. The copies already on it stay there.
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
