import { useState } from 'react';
import { api, timeoutSignal } from '../api/client';
import type { components } from '../api/schema';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { ChoiceField } from '../components/ChoiceField';
import { Field } from '../components/Field';
import { SummaryRow } from '../components/SummaryRow';
import { DependencyChoices } from '../dependencies/DependencyChoices';
import { dependenciesRequest, dependenciesText, dependencyAnswersOf, type DependenciesAnswers } from '../dependencies/dependencies';
import { attempt } from '../setup/failure';
import { refusalOf, restoreTimeoutMs, type Refusal } from './restoreApi';

type Summary = components['schemas']['RestoreSummary'];

type SummaryStepProps = Readonly<{ summary: Summary; onStarted: () => void; onBack: () => void }>;

function databaseText(summary: Summary) {
  const kind = summary.database === 'managed' ? 'The managed database' : 'An external database';
  return summary.databaseIncluded ? `${kind}, included` : `${kind}, not included: its owner restores it`;
}

/** The warning that the dump replaces the content of the external database the restore loads it into. */
function externalDatabaseWarning(summary: Summary, database: DependenciesAnswers['database']) {
  if (!summary.databaseIncluded || database.mode !== 'external') return undefined;
  return `The backup's database dump replaces the content of the external database ${database.host}:${database.port}/${database.name}. ` +
    'To test a backup while the appliance it comes from still runs, choose the dependencies anew and enter another database.';
}

/** What the restore of a checked backup does, with the ILM host name to use, and the button that starts it. */
export function SummaryStep({ summary, onStarted, onBack }: SummaryStepProps) {
  const [host, setHost] = useState(summary.ilmHostName);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<Refusal>();
  const backedUp = dependencyAnswersOf(summary.dependencies);
  // Chosen anew, the dependencies replace the backup's; their credentials are entered again.
  const [dependencies, setDependencies] = useState<DependenciesAnswers>();
  // A refused dependency shows at its field while the dependencies are chosen anew, otherwise above the form.
  const dependencyError = dependencies !== undefined && refusal?.field?.startsWith('dependencies.');
  const databaseWarning = externalDatabaseWarning(summary, (dependencies ?? backedUp).database);

  async function start() {
    setBusy(true);
    setRefusal(undefined);
    const body = {
      inspection: summary.inspection, inPlace: false, ilmHostName: host.trim(), confirm: true,
      ...(dependencies && { dependencies: dependenciesRequest(dependencies) }),
    };
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
        <SummaryRow term="Dependencies">{dependenciesText(backedUp)}</SummaryRow>
        <SummaryRow term="Console accounts added">{summary.accounts.join(', ') || 'None'}</SummaryRow>
      </dl>
      <p className="mb-4 text-sm text-content-muted">
        The host name in the backup is named in the backup's manifest, which does not prove where the backup comes from. This node keeps its own host name and network settings. The operator and appliance versions are recorded in the backup and need not match this appliance's.
      </p>
      {summary.clashes.length > 0 && (
        <Alert tone="warning">{`These accounts exist on this appliance too; the backup's account wins: ${summary.clashes.join(', ')}.`}</Alert>
      )}
      {databaseWarning && <Alert tone="warning">{databaseWarning}</Alert>}
      {refusal && refusal.field !== 'ilmHostName' && !dependencyError && <Alert tone="error">{refusal.message}</Alert>}
      <Field label="ILM host name" name="ilmHostName" value={host} onChange={(event) => setHost(event.target.value)}
        autoComplete="off" spellCheck={false} error={refusal?.field === 'ilmHostName' ? refusal.message : undefined}
        hint="The backup's ILM host name; enter another to restore ILM under a new name." />
      <ChoiceField type="checkbox" label="Choose the dependencies anew"
        description="Keep the backup's database, messaging and identity provider, or choose managed or external ones; the database dump loads into either."
        checked={dependencies !== undefined} onChange={(e) => setDependencies(e.target.checked ? backedUp : undefined)} />
      {dependencies && (
        <div className="mt-4">
          <p className="mb-4 text-sm text-content-muted">Enter the passwords and client secret of external ones again: the backup's stay sealed until the restore.</p>
          <DependencyChoices answers={dependencies} errors={dependencyError && refusal ? { [refusal.field ?? '']: refusal.message } : {}}
            onChange={setDependencies} />
        </div>
      )}
      <div className="flex flex-col gap-2 sm:flex-row">
        <Button busy={busy} disabled={!host.trim()} onClick={() => void start()}>Restore the backup</Button>
        <Button variant="secondary" disabled={busy} onClick={onBack}>Choose another backup</Button>
      </div>
    </>
  );
}
