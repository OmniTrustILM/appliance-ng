import { Link } from 'react-router';
import { Alert } from '../../components/Alert';
import { Card } from '../../components/Card';
import { reasonText } from '../backups/reasons';
import type { Status } from './useStatus';

const LINK = 'text-brand hover:underline';
/** How long after the last successful restore test the dashboard reminds the administrator of the next one. */
export const restoreTestDueMs = 90 * 24 * 60 * 60 * 1000;

/** The reminder to test a restore: none recorded, or the last one older than restoreTestDueMs at now. */
function restoreTestNotice(status: Status, now: number) {
  const last = status.backup?.lastRestoreTest;
  if (!status.backup || status.setup !== 'complete') return undefined;
  if (!last) return 'No restore test is recorded. Test that a backup restores before you need it.';
  if (now - new Date(last).getTime() > restoreTestDueMs) {
    return 'The last restore test is more than 90 days old. Test that a backup still restores.';
  }
  return undefined;
}

/** The backup warnings: failing backups, overdue targets and the restore-test reminder, judged at now. */
export function BackupWarnings({ status, now }: Readonly<{ status: Status; now: number }>) {
  const healthy = status.conditions.find((c) => c.type === 'BackupHealthy');
  const overdue = status.backup?.targets.filter((t) => t.warning) ?? [];
  const restoreTest = restoreTestNotice(status, now);
  const open = <Link className={LINK} to="/backups">Open the backups page</Link>;
  return (
    <>
      {healthy?.status === 'False' && (
        <Alert tone="warning">Backups are not healthy: {healthy.message ?? healthy.reason}. {open}</Alert>
      )}
      {overdue.map((t) => (
        <Alert key={t.name} tone="warning">
          The backup target {t.name} has had no successful copy for more than twice its interval. {open}
        </Alert>
      ))}
      {restoreTest && <Alert tone="warning">{restoreTest}</Alert>}
    </>
  );
}

/** The last backup and each target's last success and error. */
export function BackupCard({ backup }: Readonly<{ backup: NonNullable<Status['backup']> }>) {
  const last = backup.last;
  return (
    <Card>
      <h2 className="mb-3 text-base font-semibold">Backups</h2>
      <p className="text-sm">
        {last ? <>Last backup {new Date(last.time).toLocaleString()}</> : 'No backup was taken yet.'}
        {backup.running && ' A backup is running.'}
      </p>
      {last?.database === 'notIncluded' && (
        <p className="mt-1 text-sm text-warning">It holds no database: the external database's owner must back it up.</p>
      )}
      <ul className="m-0 mt-3 list-none p-0">
        {backup.targets.map((t) => (
          <li key={t.name} className="border-b border-divider py-2 text-sm last:border-b-0">
            <span className="font-medium">{t.name}</span>
            <span className="text-content-muted"> · last success {t.lastSuccess ? new Date(t.lastSuccess).toLocaleString() : 'never'}</span>
            {t.lastError && <span className="block text-danger">{reasonText(t.lastError)}</span>}
          </li>
        ))}
      </ul>
      <p className="mt-3 text-sm"><Link className={LINK} to="/backups">Manage the backups</Link></p>
    </Card>
  );
}
