import { useState, type SubmitEvent } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ChoiceField } from '../components/ChoiceField';
import { PageHeading } from '../components/PageHeading';
import { attempt, failureMessage, unreachable } from './failure';

export type Mode = 'new' | 'restore';

/** Asks whether this is a new appliance or one that restores a backup. */
export function ModePage({ onDone, onExpired }: Readonly<{ onDone: (mode: Mode) => void; onExpired: () => void }>) {
  const [mode, setMode] = useState<Mode>('new');
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    const result = await attempt(() => api.PUT('/setup/mode', { body: { mode } }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) return onDone(mode);
    if (result.response.status === 401) return onExpired();
    setError(failureMessage(result.response.status));
  }

  return (
    <form onSubmit={submit} noValidate>
      <Card actions={<Button type="submit" busy={busy}>Continue</Button>}>
        <PageHeading eyebrow="Setup" title="What do you want to set up?" lead="Both ways ask for an administrator and the settings of this node." />
        <ChoiceField
          type="radio"
          name="mode"
          label="New appliance"
          description="Choose the ILM components in the next part of the setup."
          checked={mode === 'new'}
          onChange={() => setMode('new')}
        />
        <ChoiceField
          type="radio"
          name="mode"
          label="Restore from a backup"
          description="Sign in after the installation and restore a backup with the recovery kit."
          checked={mode === 'restore'}
          onChange={() => setMode('restore')}
        />
        {error && <Alert tone="error">{error}</Alert>}
      </Card>
    </form>
  );
}
