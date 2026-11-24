import { useState, type ReactNode, type SubmitEvent } from 'react';
import { Link } from 'react-router';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ChoiceField } from '../../components/ChoiceField';
import { ComponentChoices, toggled, type ComponentChoice } from '../../components/ComponentChoices';
import { Field } from '../../components/Field';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { DependencyChoices } from '../../dependencies/DependencyChoices';
import { dependenciesRequest, dependencyAnswersOf, sentDependencies, type DependenciesAnswers } from '../../dependencies/dependencies';
import { attempt, unreachable } from '../../setup/failure';
import { useLoaded } from '../shared/useLoaded';
import { CBOMFields, cbomAnswersOf, cbomRequest, type CBOMAnswers } from './CBOMFields';

type Platform = components['schemas']['PlatformState'];
type Component = Platform['components'][number];

const link = 'text-brand hover:underline';
const loadPlatform = () => api.GET('/platform');

/** What a component still needs, or undefined when it needs nothing. */
function hintOf({ chosen, needsCredentials, needsMail, registered, url }: Component): ReactNode {
  if (!needsCredentials && !needsMail && registered) return undefined;
  return (
    <>
      {needsCredentials && (
        <><Link className={link} to="/registries">Add the registry credentials</Link>{chosen ? ', or remove it from the components.' : ' to choose it.'} </>
      )}
      {needsMail && (
        <><Link className={link} to="/mail">Set up the mail server</Link>{chosen ? '; it sends mail.' : ' before you choose it; it sends mail.'} </>
      )}
      {!registered && (
        <>Not registered: ILM does not register it by itself.{url && <> Add it as a connector in ILM with the address <code>{url}</code>.</>}</>
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

type Edits = { base?: Platform; hostName?: string; chosen?: string[]; dependencies?: DependenciesAnswers; utils?: boolean; cbom?: CBOMAnswers };
type Change = Omit<Edits, 'base'> & { hostName: string; chosen: string[] };

/** The request body: the dependencies, utils and the CBOM repository only when they were edited, so that the others
 * stay as they are. */
function updateOf({ hostName, chosen, dependencies, utils, cbom }: Change): components['schemas']['PlatformUpdate'] {
  return {
    hostName: hostName.trim(),
    components: chosen,
    ...(dependencies && { dependencies: dependenciesRequest(dependencies) }),
    ...(utils !== undefined && { utils }),
    ...(cbom && { cbom: cbomRequest(cbom) }),
  };
}

/** Shows the ILM version and changes the ILM host name, the installed components, the dependencies, utils and the CBOM
 * repository. */
export function PlatformPage() {
  const { data, error: loadError, reload } = useLoaded(loadPlatform);
  // Edits apply to the state they were made on, so that they last until the state saved with them is loaded.
  const [edits, setEdits] = useState<Edits>({});
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [hostError, setHostError] = useState<string>();
  const [fieldErrors, setFieldErrors] = useState<Partial<Record<string, string>>>({});
  const [error, setError] = useState<string>();

  async function save(change: Change) {
    setBusy(true);
    setSaved(false);
    setHostError(undefined);
    setFieldErrors({});
    setError(undefined);
    const result = await attempt(() => api.PUT('/platform', { body: updateOf(change) }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) {
      setSaved(true);
      // The appliance keeps the secrets now; the page does not, even when it cannot reload.
      setEdits((e) => ({
        ...e,
        dependencies: e.dependencies && sentDependencies(e.dependencies),
        cbom: e.cbom && { ...e.cbom, accessKey: '', secretKey: '', ca: '' },
      }));
      return reload();
    }
    const failure = result.error;
    const message = failure?.message ?? 'The appliance could not process the request.';
    if (failure?.field === 'hostName') return setHostError(message);
    if (failure?.field?.startsWith('dependencies.') || failure?.field?.startsWith('cbom.')) return setFieldErrors({ [failure.field]: message });
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
  const dependencies = current.dependencies ?? dependencyAnswersOf(data.dependencies);
  const utils = current.utils ?? data.utils;
  const cbom = current.cbom ?? cbomAnswersOf(data.cbom);

  function submit(event: SubmitEvent) {
    event.preventDefault();
    void save({ hostName, chosen, dependencies: current.dependencies, utils: current.utils, cbom: current.cbom });
  }

  return (
    <PageWidth>
      <form onSubmit={submit} noValidate>
        {/* Nothing is edited while a save is pending: its answer clears the secrets it sent. */}
        <fieldset disabled={busy} className="min-w-0">
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
            <ChoiceField type="checkbox" label="Utils service" description="Installs the utils service with the ILM."
              checked={utils} onChange={(e) => edit({ utils: e.target.checked })} />
            <CBOMFields answers={cbom} errors={fieldErrors} onChange={(answers) => edit({ cbom: answers })} />
            <h2 className="mt-6 mb-1.5 text-lg font-semibold">Dependencies</h2>
            <p className="mb-4 text-content-muted">The database, the message broker and the identity provider ILM uses.</p>
            <DependencyChoices answers={dependencies} locked={data.setupComplete} errors={fieldErrors}
              onChange={(answers) => edit({ dependencies: answers })} />
          </Card>
        </fieldset>
      </form>
    </PageWidth>
  );
}
