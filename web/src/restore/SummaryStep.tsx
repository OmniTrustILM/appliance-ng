import { useState } from 'react';
import { api, timeoutSignal } from '../api/client';
import type { components } from '../api/schema';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Field } from '../components/Field';
import { SummaryRow } from '../components/SummaryRow';
import { attempt } from '../setup/failure';
import { refusalOf, restoreTimeoutMs, type Refusal } from './restoreApi';

type Summary = components['schemas']['RestoreSummary'];

type SummaryStepProps = Readonly<{ summary: Summary; onStarted: () => void; onBack: () => void }>;

function databaseText(summary: Summary) {
  const kind = summary.database === 'managed' ? 'The managed database' : 'An external database';
  return summary.databaseIncluded ? `${kind}, included` : `${kind}, not included: its owner restores it`;
}

/** What the restore of a checked backup does, with the ILM host name to use, and the button that starts it. */
export function SummaryStep({ summary, onStarted, onBack }: SummaryStepProps) {
  const [host, setHost] = useState(summary.ilmHostName);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<Refusal>();

  async function start() {
    setBusy(true);
    setRefusal(undefined);
    const body = { inspection: summary.inspection, inPlace: false, ilmHostName: host.trim(), confirm: true };
    const refused = refusalOf(await attempt(() => api.POST('/restore/start', { body, signal: timeoutSignal(restoreTimeoutMs) })));
    setBusy(false);
    if (refused) return setRefusal(refused);
    onStarted();
  }

  return (
    <>
      <h2 className="mb-1.5 text-lg font-semibold">What the restore does</h2>
      <p className="mb-4 text-content-muted">The backup checked out with the kit. The restore replaces this appliance's settings, secrets and ILM data with the backup's.</p>
      <dl className="mb-5">
        <SummaryRow term="Backup" mono>{summary.backup}</SummaryRow>
        <SummaryRow term="Taken">{new Date(summary.time).toLocaleString()}</SummaryRow>
        <SummaryRow term="Host name in the backup">{summary.hostName}</SummaryRow>
        <SummaryRow term="This node's host name">{summary.nodeHostName}</SummaryRow>
        <SummaryRow term="ILM version">{summary.ilm}</SummaryRow>
        <SummaryRow term="Operator version in the backup">{summary.operator}</SummaryRow>
        <SummaryRow term="Appliance version in the backup">{summary.appliance}</SummaryRow>
        <SummaryRow term="Components">{summary.components.join(', ') || 'None'}</SummaryRow>
        <SummaryRow term="Database">{databaseText(summary)}</SummaryRow>
        <SummaryRow term="Console accounts added">{summary.accounts.join(', ') || 'None'}</SummaryRow>
      </dl>
      <p className="mb-4 text-sm text-content-muted">
        The host name in the backup is named in the backup's manifest, which does not prove where the backup comes from. This node keeps its own host name and network settings. The operator and appliance versions are recorded in the backup and need not match this appliance's.
      </p>
      {summary.clashes.length > 0 && (
        <Alert tone="warning">{`These accounts exist on this appliance too; the backup's account wins: ${summary.clashes.join(', ')}.`}</Alert>
      )}
      {refusal && refusal.field !== 'ilmHostName' && <Alert tone="error">{refusal.message}</Alert>}
      <Field label="ILM host name" name="ilmHostName" value={host} onChange={(event) => setHost(event.target.value)}
        autoComplete="off" spellCheck={false} error={refusal?.field === 'ilmHostName' ? refusal.message : undefined}
        hint="The backup's ILM host name; enter another to restore ILM under a new name." />
      <div className="flex flex-col gap-2 sm:flex-row">
        <Button busy={busy} disabled={!host.trim()} onClick={() => void start()}>Restore the backup</Button>
        <Button variant="secondary" disabled={busy} onClick={onBack}>Choose another backup</Button>
      </div>
    </>
  );
}
