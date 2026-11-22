import { useEffect, useRef, useState } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, unreachable } from '../../setup/failure';
import { useStatus, type Status } from '../dashboard/useStatus';

type Action = 'reboot' | 'shutdown';
type Outcome = { tone: 'success' | 'error'; text: string };

const actions = {
  reboot: {
    title: 'Restart the node?',
    button: 'Restart',
    warning: 'The console and ILM are unavailable until the node is back, which takes a few minutes.',
    done: 'The node is restarting. The console is back in a few minutes; reload the page then.',
    call: () => api.POST('/system/reboot', { body: { confirm: true } }),
  },
  shutdown: {
    title: 'Shut the node down?',
    button: 'Shut down',
    warning: 'The console and ILM go away and stay unavailable until someone starts the node again.',
    done: 'The node is shutting down. The console goes away; start the node again to use it.',
    call: () => api.POST('/system/shutdown', { body: { confirm: true } }),
  },
} as const;

/**
 * Whether the node needs a restart, once the status is read; until then the read's failure or a spinner.
 * While the latest read fails, the retained status is only the last known one.
 */
function RebootState({ status, error }: Readonly<{ status?: Status; error?: string }>) {
  if (status && error) {
    const lastKnown = status.rebootRequired ? 'a restart was needed' : 'no restart was needed';
    return (
      <Alert tone="warning">
        The node status could not be refreshed, so it is not known whether a restart is needed now. Last known: {lastKnown}.
      </Alert>
    );
  }
  if (status?.rebootRequired) return <Alert tone="warning">The node needs to restart to apply a change.</Alert>;
  if (status) return <p className="mb-5 text-sm text-content-muted">No restart is needed.</p>;
  if (error) return <Alert tone="error">{error}</Alert>;
  return <Loading />;
}

/** Shows whether the node needs a restart and restarts or shuts it down after a confirmation. */
export function SystemPage() {
  const { status, error } = useStatus();
  const [asking, setAsking] = useState<Action>();
  const [running, setRunning] = useState<Action>();
  const [outcome, setOutcome] = useState<Outcome>();
  const outcomeBox = useRef<HTMLDivElement>(null);

  // The button that opened the dialog is disabled while the action runs, so the focus moves to its outcome.
  useEffect(() => {
    if (outcome) outcomeBox.current?.focus();
  }, [outcome]);

  async function run(action: Action) {
    setAsking(undefined);
    setRunning(action);
    setOutcome(undefined);
    const result = await attempt(actions[action].call);
    setRunning(undefined);
    if (!result) return setOutcome({ tone: 'error', text: unreachable });
    if (result.response.ok) return setOutcome({ tone: 'success', text: actions[action].done });
    const text = result.response.status === 503
      ? 'The node cannot be reached at the moment. Try again in a few minutes.'
      : (result.error?.message ?? 'The appliance could not process the request.');
    setOutcome({ tone: 'error', text });
  }

  return (
    <PageWidth>
      <Card>
        <PageHeading eyebrow="Platform" title="System" lead="Restart or shut down the node." />
        <RebootState status={status} error={error} />
        <div ref={outcomeBox} tabIndex={-1} className="outline-none">
          {outcome && <Alert tone={outcome.tone}>{outcome.text}</Alert>}
        </div>
        <div className="flex flex-col gap-2 sm:flex-row">
          <Button busy={running === 'reboot'} disabled={running !== undefined} onClick={() => setAsking('reboot')}>Restart</Button>
          <Button variant="secondary" busy={running === 'shutdown'} disabled={running !== undefined} onClick={() => setAsking('shutdown')}>Shut down</Button>
        </div>
      </Card>
      {asking && (
        <ConfirmDialog
          title={actions[asking].title}
          confirmLabel={actions[asking].button}
          onConfirm={() => void run(asking)}
          onCancel={() => setAsking(undefined)}
        >
          {actions[asking].warning}
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
