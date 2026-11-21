import { useState, type ReactNode, type SubmitEvent } from 'react';
import { Link } from 'react-router';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ComponentChoices, toggled, type ComponentChoice } from '../../components/ComponentChoices';
import { Field } from '../../components/Field';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, unreachable } from '../../setup/failure';
import { useLoaded } from '../shared/useLoaded';

type Platform = components['schemas']['PlatformState'];
type Component = Platform['components'][number];

const link = 'text-brand hover:underline';
const loadPlatform = () => api.GET('/platform');

/** What a component still needs, or undefined when it needs nothing. */
function hintOf({ chosen, needsCredentials, needsMail }: Component): ReactNode {
  if (!needsCredentials && !needsMail) return undefined;
  return (
    <>
      {needsCredentials && (
        <><Link className={link} to="/registries">Add the registry credentials</Link>{chosen ? ', or remove it from the components.' : ' to choose it.'} </>
      )}
      {needsMail && (
        <><Link className={link} to="/mail">Set up the mail server</Link>{chosen ? '; it sends mail.' : ' before you choose it; it sends mail.'}</>
      )}
    </>
  );
}

/** A component that needs registry credentials cannot be chosen anew; a chosen one stays enabled to be removed. */
function choiceOf(component: Component): ComponentChoice {
  const { name, title, chosen, needsCredentials } = component;
  return {
    name,
    title,
    disabled: needsCredentials && !chosen,
    badge: needsCredentials && <Badge tone="muted">Needs registry credentials</Badge>,
    description: hintOf(component),
  };
}

type Edits = { base?: Platform; hostName?: string; chosen?: string[] };

/** Shows the ILM version and changes the ILM host name and the installed components. */
export function PlatformPage() {
  const { data, error: loadError, reload } = useLoaded(loadPlatform);
  // Edits apply to the state they were made on, so that they last until the state saved with them is loaded.
  const [edits, setEdits] = useState<Edits>({});
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [hostError, setHostError] = useState<string>();
  const [error, setError] = useState<string>();

  async function save(hostName: string, chosen: string[]) {
    setBusy(true);
    setSaved(false);
    setHostError(undefined);
    setError(undefined);
    const result = await attempt(() => api.PUT('/platform', { body: { hostName: hostName.trim(), components: chosen } }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) {
      setSaved(true);
      return reload();
    }
    const failure = result.error;
    const message = failure?.message ?? 'The appliance could not process the request.';
    if (failure?.field === 'hostName') return setHostError(message);
    setError(message);
  }

  if (!data) {
    return (
      <PageWidth>
        <Card>{loadError ? <Alert tone="error">{loadError}</Alert> : <Loading />}</Card>
      </PageWidth>
    );
  }

  const current = edits.base === data ? edits : {};
  const edit = (change: Omit<Edits, 'base'>) => setEdits({ ...current, ...change, base: data });
  const hostName = current.hostName ?? data.hostName;
  const chosen = current.chosen ?? data.components.filter((c) => c.chosen).map((c) => c.name);

  function submit(event: SubmitEvent) {
    event.preventDefault();
    void save(hostName, chosen);
  }

  return (
    <PageWidth>
      <form onSubmit={submit} noValidate>
        <Card actions={<Button type="submit" busy={busy}>Save</Button>}>
          <PageHeading eyebrow="Platform" title="Components" lead="The ILM host name and the components that are installed with the ILM." />
          {saved && (
            <Alert tone="success">
              The change is saved. The components install in the background; the <Link className={link} to="/">dashboard</Link> shows the progress.
            </Alert>
          )}
          {error && <Alert tone="error">{error}</Alert>}
          <Field label="ILM version" name="ilmVersion" value={data.ilmVersion} readOnly />
          <Field label="ILM host name" name="hostName" value={hostName} error={hostError} autoComplete="off" spellCheck={false}
            onChange={(e) => edit({ hostName: e.target.value })} />
          <ComponentChoices
            choices={data.components.map(choiceOf)}
            chosen={chosen}
            onToggle={(name, checked) => edit({ chosen: toggled(chosen, name, checked) })}
          />
        </Card>
      </form>
    </PageWidth>
  );
}
