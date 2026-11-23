import { useState } from 'react';
import type { components } from '../api/schema';
import { Button } from '../components/Button';
import { attempt } from '../setup/failure';
import { KitForm } from './KitForm';
import { inspectBackup, refusalOf } from './restoreApi';
import { SourceStep } from './SourceStep';
import { SummaryStep } from './SummaryStep';

type Summary = components['schemas']['RestoreSummary'];

/** The restore onto this fresh appliance: the backup, the kit that opens it, then what the restore does. */
export function FreshRestore({ onStarted }: Readonly<{ onStarted: () => void }>) {
  const [name, setName] = useState<string>();
  const [summary, setSummary] = useState<Summary>();

  function startOver() {
    setSummary(undefined);
    setName(undefined);
  }

  async function check(kit: File, passphrase: string) {
    const result = await attempt(() => inspectBackup('upload', name ?? '', kit, passphrase));
    if (result?.data) setSummary(result.data);
    return refusalOf(result);
  }

  if (summary) return <SummaryStep summary={summary} onStarted={onStarted} onBack={startOver} />;
  if (name) {
    return (
      <>
        <p className="mb-4 text-content-muted">
          <span className="font-mono text-[13px]">{name}</span> is on the appliance. The recovery kit and its passphrase open it; the whole backup is checked before anything changes.
        </p>
        <KitForm label="Check the backup" submitLabel="Check the backup" onSubmit={check}>
          <Button variant="secondary" onClick={startOver}>Choose another backup</Button>
        </KitForm>
      </>
    );
  }
  return <SourceStep onReady={setName} />;
}
