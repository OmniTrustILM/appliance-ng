import { useState, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Field } from '../../components/Field';
import { attempt, unreachable } from '../../setup/failure';

export type Registry = components['schemas']['RegistryCredentials'];

type FieldName = 'host' | 'username' | 'password';

type RegistryFormProps = Readonly<{
  editing?: Registry;
  onSaved: () => void;
  onCancel: () => void;
}>;

const fields: ReadonlySet<string> = new Set(['host', 'username', 'password']);

/** Adds a registry or, when `editing`, changes the credentials of one; the server's field errors show at their field. */
export function RegistryForm({ editing, onSaved, onCancel }: RegistryFormProps) {
  const [host, setHost] = useState(editing?.host ?? '');
  const [username, setUsername] = useState(editing?.username ?? '');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<Partial<Record<FieldName, string>>>({});
  const [error, setError] = useState<string>();

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    setFieldErrors({});
    const result = await attempt(() => api.PUT('/registries/{host}', {
      params: { path: { host: host.trim() } },
      body: password ? { username, password } : { username },
    }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) return onSaved();
    const failure = result.error;
    const message = failure?.message ?? 'The appliance could not process the request.';
    if (failure?.field && fields.has(failure.field)) return setFieldErrors({ [failure.field]: message });
    setError(message);
  }

  const keepHint = editing?.passwordSet ? 'Leave empty to keep the current password.' : undefined;

  return (
    <form onSubmit={(event) => void submit(event)} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <Field label="Registry host" name="host" value={host} onChange={(event) => setHost(event.target.value)}
        readOnly={Boolean(editing)} error={fieldErrors.host} autoComplete="off" spellCheck={false}
        hint="A host name or IP address with an optional port, such as registry.example.com:5000." />
      <Field label="User name" name="username" value={username} onChange={(event) => setUsername(event.target.value)}
        error={fieldErrors.username} autoComplete="off" spellCheck={false} />
      <Field label="Password" name="password" type="password" value={password}
        onChange={(event) => setPassword(event.target.value)} error={fieldErrors.password}
        autoComplete="new-password" hint={keepHint} />
      <div className="flex flex-col gap-2 sm:flex-row">
        <Button type="submit" busy={busy} disabled={!host.trim() || !username}>{editing ? 'Save the credentials' : 'Add the registry'}</Button>
        {editing && <Button variant="secondary" onClick={onCancel}>Cancel</Button>}
      </div>
    </form>
  );
}
