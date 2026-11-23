import { useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Button, buttonClass } from '../../components/Button';
import { apiBase } from '../../base';
import { attempt, unreachable } from '../../setup/failure';
import { formatBytes } from '../shared/format';
import { ItemRow } from '../shared/ItemRow';

type Backups = components['schemas']['Backups'];

const started = 'The backup started. It is listed here once it is written; the targets get their copies after that.';
const noDatabase = 'The database did not answer, so this backup holds no database. An external database must be backed up by its owner.';

/** The backups on the data volume, newest first, with their downloads and "Back up now". */
export function LocalBackups({ backups, onStarted }: Readonly<{ backups: Backups; onStarted: () => void }>) {
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState(false);
  const [error, setError] = useState<string>();

  async function backUp() {
    setBusy(true);
    setNotice(false);
    setError(undefined);
    const result = await attempt(() => api.POST('/backups'));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (!result.response.ok) return setError(result.error?.message ?? 'The backup could not be started.');
    setNotice(true);
    onStarted();
  }

  return (
    <>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-lg font-semibold">On the appliance</h2>
        <div className="flex items-center gap-3">
          {backups.running && <Badge tone="wait">A backup is running</Badge>}
          <Button busy={busy} disabled={backups.running} onClick={() => void backUp()}>Back up now</Button>
        </div>
      </div>
      {notice && <Alert tone="success">{started}</Alert>}
      {error && <Alert tone="error">{error}</Alert>}
      {backups.local.length === 0 && <p className="text-content-muted">No backup was taken yet.</p>}
      <ul className="m-0 list-none p-0">
        {backups.local.map((b) => (
          <ItemRow
            key={b.name}
            actions={(
              <a className={buttonClass('secondary')} href={`${apiBase}/backups/${encodeURIComponent(b.name)}`} download aria-label={`Download ${b.name}`}>
                Download
              </a>
            )}
          >
            <p className="m-0 font-mono text-[13px]">{b.name}</p>
            <p className="m-0 text-content-muted">{new Date(b.time).toLocaleString()} · {formatBytes(b.sizeBytes)}</p>
            {b.database === 'notIncluded' && (
              <p className="m-0 mt-1"><Badge tone="wait">Database not included</Badge> <span className="text-content-muted">{noDatabase}</span></p>
            )}
          </ItemRow>
        ))}
      </ul>
    </>
  );
}
