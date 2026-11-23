import { useState } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { attempt, unreachable } from '../../setup/failure';
import { ItemRow } from '../shared/ItemRow';
import { reasonText, type Reason } from './reasons';
import type { Target } from './TargetForm';

/** Where the target keeps the backups. */
function location(target: Target) {
  if (target.s3) return `${target.s3.endpoint} · ${target.s3.bucket}${target.s3.prefix ? `/${target.s3.prefix}` : ''}`;
  if (target.sftp) return `${target.sftp.user}@${target.sftp.host}:${target.sftp.path}`;
  if (target.nfs) return `${target.nfs.server}:${target.nfs.export}${target.nfs.path ? `/${target.nfs.path}` : ''}`;
  return '';
}

/** The health of a target: its last copy failed, it is overdue, it has copies, or it has none yet. */
export function HealthBadge({ health }: Readonly<{ health: Target['health'] }>) {
  if (health.lastError) return <Badge tone="bad">Last copy failed</Badge>;
  if (health.warning) return <Badge tone="wait">Overdue</Badge>;
  if (health.lastSuccess) return <Badge tone="ok">Healthy</Badge>;
  return <Badge tone="muted">No copy yet</Badge>;
}

type TestResult = { ok: boolean; reason?: Reason } | { unreachable: true };

function TestOutcome({ result }: Readonly<{ result: TestResult }>) {
  if ('unreachable' in result) return <Alert tone="error">{unreachable}</Alert>;
  if (result.ok) return <Alert tone="success">The test succeeded: the target took a test file, returned it and deleted it.</Alert>;
  return <Alert tone="error">The test failed. {result.reason ? reasonText(result.reason) : ''}</Alert>;
}

type TargetRowProps = Readonly<{ target: Target; onEdit: () => void; onRemove: () => void }>;

/** A target with its settings, health, test and actions. */
export function TargetRow({ target, onEdit, onRemove }: TargetRowProps) {
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<TestResult>();
  const { health } = target;

  async function test() {
    setTesting(true);
    setResult(undefined);
    const sent = await attempt(() => api.POST('/backups/targets/{name}/test', { params: { path: { name: target.name } } }));
    setTesting(false);
    if (sent?.data) return setResult(sent.data);
    setResult(sent ? { ok: false } : { unreachable: true });
  }

  return (
    <ItemRow
      actions={(
        <>
          <Button variant="secondary" busy={testing} aria-label={`Test ${target.name}`} onClick={() => void test()}>Test</Button>
          <Button variant="secondary" aria-label={`Edit ${target.name}`} onClick={onEdit}>Edit</Button>
          <Button variant="secondary" aria-label={`Remove ${target.name}`} onClick={onRemove}>Remove</Button>
        </>
      )}
    >
      <p className="m-0 flex flex-wrap items-center gap-2 font-medium">
        {target.name} <span className="text-xs uppercase text-content-subtle">{target.type}</span> <HealthBadge health={health} />
      </p>
      <p className="m-0 font-mono text-[13px] text-content-muted">{location(target)}</p>
      <p className="m-0 text-content-muted">
        Schedule <span className="font-mono">{target.schedule}</span> ({target.timeZone}), keeps {target.retention}
      </p>
      <p className="m-0 text-content-muted">
        Last success: {health.lastSuccess ? new Date(health.lastSuccess).toLocaleString() : 'never'}
      </p>
      {health.lastError && <p className="m-0 text-danger">Last error: {reasonText(health.lastError)}</p>}
      {health.warning && <p className="m-0 text-warning">No copy succeeded for more than twice the schedule's interval.</p>}
      {result && <div className="mt-2"><TestOutcome result={result} /></div>}
    </ItemRow>
  );
}
