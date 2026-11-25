import { useState } from 'react';
import { useNavigate } from 'react-router';
import { api } from '../api/client';
import type { components } from '../api/schema';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { attempt } from '../setup/failure';
import { KitForm } from './KitForm';
import { inspectBackup, refusalOf } from './restoreApi';
import { SourceStep } from './SourceStep';
import { SummaryStep } from './SummaryStep';
import { useLatest } from './useLatest';

type Summary = components['schemas']['RestoreSummary'];

/** How often and how far apart the status is read until it shows the setup left the restore. */
const statusReads = 10;
const statusReadMs = 300;

/** Waits until the status no longer shows the restore, which it may for a moment after the setup moved on. */
async function setupLeftRestore() {
  for (let i = 0; i < statusReads; i++) {
    const result = await attempt(() => api.GET('/status'));
    if (result?.data && result.data.setup !== 'restore') return;
    await new Promise((resolve) => setTimeout(resolve, statusReadMs));
  }
}

/** The way out of the restore: a new appliance is set up in the wizard instead. */
function SetUpNew() {
  const navigate = useNavigate();
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string>();

  async function abandon() {
    setBusy(true);
    const result = await attempt(() => api.POST('/restore/abandon'));
    setAsking(false);
    const refused = refusalOf(result);
    if (refused) {
      setBusy(false);
      return setRefusal(refused.message);
    }
    await setupLeftRestore();
    setBusy(false);
    void navigate('/wizard/');
  }

  return (
    <div className="mt-6 border-t border-divider pt-4">
      {refusal && <Alert tone="error">{refusal}</Alert>}
      <Button variant="secondary" busy={busy} onClick={() => setAsking(true)}>Set up a new appliance instead</Button>
      {asking && (
        <ConfirmDialog title="Set up a new appliance?" confirmLabel="Set up a new appliance" onConfirm={() => void abandon()} onCancel={() => setAsking(false)}>
          The backup is not restored. You choose the ILM components in the setup wizard.
        </ConfirmDialog>
      )}
    </div>
  );
}

/** The restore onto this fresh appliance: the backup, the kit that opens it, then what the restore does. */
export function FreshRestore({ onStarted }: Readonly<{ onStarted: () => void }>) {
  const [name, setName] = useState<string>();
  const [summary, setSummary] = useState<Summary>();
  const begin = useLatest();

  function startOver() {
    begin();
    setSummary(undefined);
    setName(undefined);
  }

  async function check(kit: File, passphrase: string) {
    const latest = begin();
    const result = await attempt(() => inspectBackup('upload', name ?? '', kit, passphrase));
    if (!latest()) return undefined;
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
        <SetUpNew />
      </>
    );
  }
  return (
    <>
      <SourceStep onReady={setName} />
      <SetUpNew />
    </>
  );
}
