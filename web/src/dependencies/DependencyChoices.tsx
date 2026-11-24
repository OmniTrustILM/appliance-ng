import { type ChangeEvent, useId, useRef, useState } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { ChoiceField } from '../components/ChoiceField';
import { Field, INPUT } from '../components/Field';
import { TextArea } from '../components/TextArea';
import { attempt, failureMessage, unreachable } from '../setup/failure';
import {
  kinds, reasonText, testFieldOf, testRequest, type DependenciesAnswers, type DependencyAnswers, type Kind, type Mode, type TLSMode,
} from './dependencies';

type Errors = Partial<Record<string, string>>;

const titles: Record<Kind, string> = { database: 'Database', messaging: 'Messaging', identity: 'Identity' };
const modeLabels: Record<Kind, Record<Mode, string>> = {
  database: { managed: 'Managed PostgreSQL', external: 'External PostgreSQL' },
  messaging: { managed: 'Managed RabbitMQ', external: 'External RabbitMQ' },
  identity: { managed: 'Managed Keycloak', external: 'External OIDC provider' },
};
const modeHints: Record<Kind, Record<Mode, string>> = {
  database: { managed: 'The appliance runs the database and backs it up.', external: 'ILM uses a PostgreSQL database you run.' },
  messaging: { managed: 'The appliance runs the message broker.', external: 'ILM uses a RabbitMQ broker you run.' },
  identity: { managed: 'The appliance runs Keycloak for sign-in.', external: 'Users sign in through your OIDC provider.' },
};

/** Why the mode of a dependency is fixed once setup is complete. */
export const lockedModeText =
  'Switching between managed and external moves data. Take a backup, set up a fresh appliance with the new choice and restore the backup onto it.';

const tlsModes: { value: TLSMode; label: string }[] = [
  { value: '', label: 'TLS when the server offers it, not verified' },
  { value: 'disable', label: 'No TLS' },
  { value: 'require', label: 'TLS, not verified' },
  { value: 'verify-full', label: 'TLS, verifying the server against a CA' },
];

/** What ILM's components do with the database's TLS settings. */
export const databaseTLSText =
  "ILM's core and scheduler use this setting. ILM's auth service and Keycloak encrypt the connection when the server offers TLS but do not verify the server's certificate; make the server accept only TLS to be sure every connection is encrypted.";

/** The CA field of a dependency that verifies its server. */
function CAField({ kind, answers, error, onEdit }: Readonly<{
  kind: Kind;
  answers: DependencyAnswers;
  error?: string;
  onEdit: (patch: Partial<DependencyAnswers>) => void;
}>) {
  return (
    <TextArea label={`${kind === 'database' ? 'Database' : 'Broker'} CA certificate`} name={`${kind}.ca`} value={answers.ca} error={error}
      hint={answers.caSet ? 'A CA is set. Leave it blank to keep it.' : "The PEM CA certificates that sign the server's certificate."}
      onChange={(ca) => onEdit({ ca })} />
  );
}

/** The TLS settings of the database or the broker. */
function TLSFields({ kind, answers, errors, onEdit }: Readonly<{
  kind: Kind;
  answers: DependencyAnswers;
  errors: Errors;
  onEdit: (patch: Partial<DependencyAnswers>) => void;
}>) {
  const id = useId();
  const caError = errors[`dependencies.${kind}.ca`];
  if (kind === 'messaging') {
    return (
      <>
        <ChoiceField type="checkbox" label="Connect with TLS (AMQPS)" description="Use the broker's TLS port above; ILM verifies the broker against the CA."
          checked={answers.tls} onChange={(e) => onEdit({ tls: e.target.checked })} />
        {answers.tls && <CAField kind={kind} answers={answers} error={caError} onEdit={onEdit} />}
      </>
    );
  }
  const modeError = errors['dependencies.database.tlsMode'];
  return (
    <>
      <div className="mb-[18px]">
        <label htmlFor={id} className="mb-1.5 block text-sm font-medium">Database TLS</label>
        <select id={id} name="database.tlsMode" className={INPUT} value={answers.tlsMode} onChange={(e) => onEdit({ tlsMode: e.target.value as TLSMode })}>
          {tlsModes.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
        </select>
        <p className="mt-1 text-[13px] text-content-subtle">{databaseTLSText}</p>
        {modeError && <p className="mt-1.5 text-[13px] text-danger" role="alert">{modeError}</p>}
      </div>
      {answers.tlsMode === 'verify-full' && <CAField kind={kind} answers={answers} error={caError} onEdit={onEdit} />}
    </>
  );
}

type TestState = { busy: boolean; result?: { ok: boolean; text: string } };

/** The fields of one external dependency; errors are keyed by request field, such as dependencies.database.host. */
function ExternalFields({ kind, answers, errors, onEdit }: Readonly<{
  kind: Kind;
  answers: DependencyAnswers;
  errors: Errors;
  onEdit: (patch: Partial<DependencyAnswers>) => void;
}>) {
  const field = (name: string) => errors[`dependencies.${kind}.${name}`];
  const secretHint = answers.secretSet ? 'One is set. Leave the field empty to keep it while the server, its TLS setting and the user stay the same.' : undefined;
  if (kind === 'identity') {
    return (
      <>
        <Field label="Issuer URL" name="identity.issuer" value={answers.issuer} error={field('issuer')} autoComplete="off" spellCheck={false}
          placeholder="https://idp.example.com/realms/ilm" onChange={(e) => onEdit({ issuer: e.target.value })} />
        <Field label="Client ID" name="identity.clientId" value={answers.clientId} error={field('clientId')} autoComplete="off"
          onChange={(e) => onEdit({ clientId: e.target.value })} />
        <Field label="Scopes" name="identity.scopes" value={answers.scopes} error={field('scopes')} autoComplete="off" hint="Apart by spaces, such as openid profile email."
          onChange={(e) => onEdit({ scopes: e.target.value })} />
        <Field label="Accepted audiences" name="identity.audiences" value={answers.audiences} error={field('audiences')} autoComplete="off"
          hint="Optional, apart by spaces, such as the client ID. Without audiences ILM accepts any token from this issuer, whatever client it was issued for."
          onChange={(e) => onEdit({ audiences: e.target.value })} />
        <Field label="Client secret" name="identity.clientSecret" type="password" value={answers.secret} error={field('clientSecret')}
          hint={secretHint} autoComplete="new-password" onChange={(e) => onEdit({ secret: e.target.value })} />
      </>
    );
  }
  const server = kind === 'database' ? 'Database' : 'Broker';
  return (
    <>
      <div className="grid gap-x-4 sm:grid-cols-[1fr_140px]">
        <Field label={`${server} host`} name={`${kind}.host`} value={answers.host} error={field('host')} autoComplete="off" spellCheck={false}
          onChange={(e) => onEdit({ host: e.target.value })} />
        <Field label={`${server} port`} name={`${kind}.port`} inputMode="numeric" value={answers.port} error={field('port')}
          onChange={(e) => onEdit({ port: e.target.value })} />
      </div>
      {kind === 'database' ? (
        <Field label="Database name" name="database.name" value={answers.name} error={field('name')} autoComplete="off"
          onChange={(e) => onEdit({ name: e.target.value })} />
      ) : (
        <Field label="Virtual host" name="messaging.virtualHost" value={answers.virtualHost} error={field('virtualHost')} autoComplete="off"
          onChange={(e) => onEdit({ virtualHost: e.target.value })} />
      )}
      <Field label={`${server} user name`} name={`${kind}.username`} value={answers.username} error={field('username')} autoComplete="off"
        onChange={(e) => onEdit({ username: e.target.value })} />
      <Field label={`${server} password`} name={`${kind}.password`} type="password" value={answers.secret} error={field('password')}
        hint={secretHint} autoComplete="new-password" onChange={(e) => onEdit({ secret: e.target.value })} />
      <TLSFields kind={kind} answers={answers} errors={errors} onEdit={onEdit} />
    </>
  );
}

/** One dependency: managed or external, the external settings and a test of them. A locked mode cannot change. */
function DependencyFields({ kind, answers, locked, errors, onEdit }: Readonly<{
  kind: Kind;
  answers: DependencyAnswers;
  locked: boolean;
  errors: Errors;
  onEdit: (patch: Partial<DependencyAnswers>) => void;
}>) {
  const [test, setTest] = useState<TestState>({ busy: false });
  const [testErrors, setTestErrors] = useState<Errors>({});
  // Counts the tests and edits: a test's answer counts only while nothing came after it.
  const latest = useRef(0);
  const shown = { ...errors, ...testErrors };

  function edit(patch: Partial<DependencyAnswers>) {
    latest.current++;
    setTest({ busy: false });
    setTestErrors({});
    onEdit(patch);
  }

  function chooseMode(e: ChangeEvent<HTMLInputElement>) {
    edit({ mode: e.target.value as DependencyAnswers['mode'] });
  }

  async function runTest() {
    const run = ++latest.current;
    setTest({ busy: true });
    setTestErrors({});
    const result = await attempt(() => api.POST('/platform/dependencies/test', { body: testRequest(kind, answers) }));
    if (run !== latest.current) return;
    if (!result) return setTest({ busy: false, result: { ok: false, text: unreachable } });
    const { data, error, response } = result;
    if (data) return setTest({ busy: false, result: data.ok ? { ok: true, text: 'The connection works.' } : { ok: false, text: reasonText(data.reason ?? 'failed', kind === 'database' && answers.tlsMode === 'verify-full') } });
    if (error?.field) {
      setTestErrors({ [testFieldOf(kind, error.field)]: error.message });
      return setTest({ busy: false });
    }
    setTest({ busy: false, result: { ok: false, text: error?.message ?? failureMessage(response.status) } });
  }

  return (
    <fieldset className="mb-6 min-w-0">
      <legend className="mb-2 text-base font-semibold">{titles[kind]}</legend>
      {locked ? (
        <p className="mb-3 text-content-muted">{modeLabels[kind][answers.mode]}. {lockedModeText}</p>
      ) : (
        (['managed', 'external'] as const).map((mode) => (
          <ChoiceField key={mode} type="radio" name={`${kind}-mode`} value={mode} label={modeLabels[kind][mode]} description={modeHints[kind][mode]}
            checked={answers.mode === mode} onChange={chooseMode} />
        ))
      )}
      {!locked && shown[`dependencies.${kind}.mode`] && <Alert tone="error">{shown[`dependencies.${kind}.mode`]}</Alert>}
      {answers.mode === 'external' && (
        <div className="mt-3">
          <ExternalFields kind={kind} answers={answers} errors={shown} onEdit={edit} />
          <Button variant="secondary" busy={test.busy} onClick={() => void runTest()}>{`Test the ${titles[kind].toLowerCase()} connection`}</Button>
          {test.result && <div className="mt-3"><Alert tone={test.result.ok ? 'success' : 'error'}>{test.result.text}</Alert></div>}
        </div>
      )}
    </fieldset>
  );
}

/** The database, messaging and identity provider, each managed or external. */
export function DependencyChoices({ answers, locked = false, errors, onChange }: Readonly<{
  answers: DependenciesAnswers;
  locked?: boolean;
  errors: Errors;
  onChange: (answers: DependenciesAnswers) => void;
}>) {
  return (
    <>
      {kinds.map((kind) => (
        <DependencyFields key={kind} kind={kind} answers={answers[kind]} locked={locked} errors={errors}
          onEdit={(patch) => onChange({ ...answers, [kind]: { ...answers[kind], ...patch } })} />
      ))}
    </>
  );
}
