import { useId, useState, type ReactNode, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { ChoiceField } from '../../components/ChoiceField';
import { Field, INPUT } from '../../components/Field';
import { cx } from '../../components/cx';
import { attempt, unreachable } from '../../setup/failure';
import { HostKeyStep } from './HostKeyStep';

export type Target = components['schemas']['BackupTarget'];
type Update = components['schemas']['BackupTargetUpdate'];
type Kind = Target['type'];
type NFSVersion = NonNullable<components['schemas']['NFSSettings']['version']>;

export const defaultSchedule = '0 2 * * *';
const defaultRetention = '14';
const keepHint = 'Leave blank to keep the stored value.';
export const enterAgainHint = 'Enter it again: stored secrets are used only for the server they were given for.';

/** The browser's IANA time zone, which a new target's schedule runs in. */
export function browserTimeZone() {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

type Values = {
  name: string;
  type: Kind;
  schedule: string;
  timeZone: string;
  retention: string;
  endpoint: string;
  bucket: string;
  prefix: string;
  region: string;
  accessKey: string;
  secretKey: string;
  ca: string;
  removeCa: boolean;
  host: string;
  port: string;
  path: string;
  user: string;
  hostKey: string;
  password: string;
  privateKey: string;
  server: string;
  exportPath: string;
  nfsPath: string;
  version: NFSVersion;
};

/** The fields entered as text. */
type TextKey = { [K in keyof Values]: Values[K] extends string ? K : never }[keyof Values];

function initialValues(editing?: Target): Values {
  return {
    name: editing?.name ?? '',
    type: editing?.type ?? 's3',
    schedule: editing?.schedule ?? defaultSchedule,
    timeZone: editing?.timeZone ?? browserTimeZone(),
    retention: String(editing?.retention ?? defaultRetention),
    endpoint: editing?.s3?.endpoint ?? '',
    bucket: editing?.s3?.bucket ?? '',
    prefix: editing?.s3?.prefix ?? '',
    region: editing?.s3?.region ?? '',
    accessKey: '',
    secretKey: '',
    ca: '',
    removeCa: false,
    host: editing?.sftp?.host ?? '',
    port: String(editing?.sftp?.port ?? 22),
    path: editing?.sftp?.path ?? '',
    user: editing?.sftp?.user ?? '',
    hostKey: editing?.sftp?.hostKey ?? '',
    password: '',
    privateKey: '',
    server: editing?.nfs?.server ?? '',
    exportPath: editing?.nfs?.export ?? '',
    nfsPath: editing?.nfs?.path ?? '',
    version: editing?.nfs?.version ?? '4.1',
  };
}

/** The request body; secrets left blank are left out, so that the stored ones stay. */
function updateOf(v: Values): Update {
  const blank = (s: string) => s || undefined;
  const common = { type: v.type, schedule: v.schedule.trim(), timeZone: v.timeZone.trim(), retention: Number(v.retention) };
  switch (v.type) {
    case 's3':
      return { ...common, s3: { endpoint: v.endpoint.trim(), bucket: v.bucket.trim(), prefix: blank(v.prefix), region: blank(v.region),
        accessKey: blank(v.accessKey), secretKey: blank(v.secretKey), ca: v.removeCa ? undefined : blank(v.ca),
        removeCa: v.removeCa || undefined } };
    case 'sftp':
      return { ...common, sftp: { host: v.host.trim(), port: Number(v.port), path: v.path.trim(), user: v.user, hostKey: v.hostKey,
        password: blank(v.password), privateKey: blank(v.privateKey) } };
    case 'nfs':
      return { ...common, nfs: { server: v.server.trim(), export: v.exportPath.trim(), path: blank(v.nfsPath), version: v.version } };
  }
}

/** Whether v sends the credentials elsewhere than the edited target: another type, S3 endpoint or SFTP host, port or user. */
function relocated(editing: Target, v: Values) {
  switch (v.type) {
    case 's3':
      return editing.type !== v.type || editing.s3?.endpoint !== v.endpoint.trim();
    case 'sftp':
      return editing.type !== v.type || editing.sftp?.host !== v.host.trim() || editing.sftp?.port !== Number(v.port) ||
        editing.sftp?.user !== v.user;
    case 'nfs':
      return false;
  }
}

const kinds: ReadonlyArray<{ value: Kind; label: string; description: string }> = [
  { value: 's3', label: 'S3', description: 'A bucket of an S3 compatible object store.' },
  { value: 'sftp', label: 'SFTP', description: 'A directory on an SSH server.' },
  { value: 'nfs', label: 'NFS', description: 'A directory on an NFS export.' },
];

type TextAreaProps = Readonly<{ label: string; name: string; value: string; onChange: (value: string) => void; hint?: ReactNode; error?: string }>;

function TextArea({ label, name, value, onChange, hint, error }: TextAreaProps) {
  const id = useId();
  return (
    <div className="mb-[18px]">
      <label htmlFor={id} className="mb-1.5 block text-sm font-medium">{label}</label>
      <textarea id={id} name={name} value={value} onChange={(event) => onChange(event.target.value)} rows={4}
        spellCheck={false} autoComplete="off" aria-invalid={error ? true : undefined}
        className={cx(INPUT, 'h-auto py-2 font-mono text-[13px]')} />
      {hint && <p className="mt-1 text-[13px] text-content-subtle">{hint}</p>}
      {error && <p className="mt-1.5 text-[13px] text-danger" role="alert">{error}</p>}
    </div>
  );
}

type TargetFormProps = Readonly<{
  editing?: Target;
  onSaved: () => void;
  onCancel: () => void;
}>;

/** Adds a backup target or, when `editing`, changes one; secrets are never shown and left blank keep the stored ones. */
export function TargetForm({ editing, onSaved, onCancel }: TargetFormProps) {
  const [values, setValues] = useState(() => initialValues(editing));
  const [busy, setBusy] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<string>();
  const zones = useId();
  const secrets = editing?.secretsSet;
  // Stored credentials stay with their server: sent elsewhere, they must be entered again.
  const moved = editing !== undefined && relocated(editing, values);
  const secretHint = (stored: boolean | undefined, otherwise?: string) => {
    if (!stored) return otherwise;
    return moved ? enterAgainHint : keepHint;
  };

  function set<K extends keyof Values>(key: K, value: Values[K]) {
    setValues((v) => {
      const next = { ...v, [key]: value };
      // A key accepted for one server is not accepted for another.
      if ((key === 'host' || key === 'port') && value !== v[key]) next.hostKey = '';
      return next;
    });
  }

  const text = (key: TextKey, label: string, extra: Partial<Parameters<typeof Field>[0]> = {}) => (
    <Field label={label} name={key} value={values[key]} onChange={(event) => set(key, event.target.value as never)}
      autoComplete="off" spellCheck={false} {...extra} />
  );

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    setErrors({});
    const result = await attempt(() => api.PUT('/backups/targets/{name}', {
      params: { path: { name: values.name.trim() } },
      body: updateOf(values),
    }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) return onSaved();
    const message = result.error?.message ?? 'The appliance could not process the request.';
    if (result.error?.field) return setErrors({ [result.error.field]: message });
    setError(message);
  }

  const needsHostKey = values.type === 'sftp' && !values.hostKey;

  return (
    <form onSubmit={(event) => void submit(event)} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      {text('name', 'Name', { readOnly: Boolean(editing), error: errors.name, hint: 'Lowercase letters, digits and hyphens, such as offsite.' })}
      <fieldset className="mb-[18px]">
        <legend className="mb-1.5 text-sm font-medium">Type</legend>
        {kinds.map((kind) => (
          <ChoiceField key={kind.value} type="radio" name="type" label={kind.label} description={kind.description}
            checked={values.type === kind.value} onChange={() => set('type', kind.value)} />
        ))}
        {errors.type && <p className="text-[13px] text-danger" role="alert">{errors.type}</p>}
      </fieldset>

      {values.type === 's3' && (
        <>
          {text('endpoint', 'Endpoint', { error: errors['s3.endpoint'], hint: 'https://host[:port], such as https://s3.eu-central-1.amazonaws.com.' })}
          {text('bucket', 'Bucket', { error: errors['s3.bucket'] })}
          {text('prefix', 'Prefix (optional)', { error: errors['s3.prefix'] })}
          {text('region', 'Region (optional)', { error: errors['s3.region'] })}
          {text('accessKey', 'Access key', { type: 'password', autoComplete: 'new-password', error: errors['s3.accessKey'], required: moved,
            hint: secretHint(secrets?.accessKey) })}
          {text('secretKey', 'Secret key', { type: 'password', autoComplete: 'new-password', error: errors['s3.secretKey'], required: moved,
            hint: secretHint(secrets?.secretKey) })}
          {!values.removeCa && (
            <TextArea label="CA bundle (optional)" name="ca" value={values.ca} onChange={(v) => set('ca', v)} error={errors['s3.ca']}
              hint={secrets?.ca ? 'Leave blank to keep the stored CA bundle.' : 'The PEM CA certificates that sign the endpoint, when they are not publicly trusted.'} />
          )}
          {secrets?.ca && (
            <ChoiceField type="checkbox" name="removeCa" label="Remove the stored CA bundle" checked={values.removeCa}
              description="The endpoint's certificate must then be publicly trusted." onChange={(event) => set('removeCa', event.target.checked)} />
          )}
        </>
      )}

      {values.type === 'sftp' && (
        <>
          {text('host', 'Host', { error: errors['sftp.host'] })}
          {text('port', 'Port', { inputMode: 'numeric', error: errors['sftp.port'] })}
          {text('path', 'Directory', { error: errors['sftp.path'], hint: 'The directory on the server the backups go to.' })}
          {text('user', 'User name', { error: errors['sftp.user'] })}
          {text('password', 'Password', { type: 'password', autoComplete: 'new-password', error: errors['sftp.password'],
            required: moved && !values.privateKey, hint: secretHint(secrets?.password, 'Enter a password or a private key.') })}
          <TextArea label="Private key" name="privateKey" value={values.privateKey} onChange={(v) => set('privateKey', v)} error={errors['sftp.privateKey']}
            hint={secretHint(secrets?.privateKey, 'An OpenSSH or PEM private key without a passphrase.')} />
          <HostKeyStep host={values.host} port={Number(values.port)} accepted={values.hostKey} error={errors['sftp.hostKey']}
            onAccept={(fingerprint) => set('hostKey', fingerprint)} />
        </>
      )}

      {values.type === 'nfs' && (
        <>
          {text('server', 'Server', { error: errors['nfs.server'] })}
          {text('exportPath', 'Export', { error: errors['nfs.export'], hint: 'The exported path, such as /exports/backups.' })}
          {text('nfsPath', 'Directory (optional)', { error: errors['nfs.path'], hint: 'A directory below the export.' })}
          <div className="mb-[18px]">
            <label htmlFor={`${zones}-version`} className="mb-1.5 block text-sm font-medium">NFS version</label>
            <select id={`${zones}-version`} name="version" value={values.version} className={INPUT}
              onChange={(event) => set('version', event.target.value as NFSVersion)}>
              <option value="4.1">4.1</option>
              <option value="4.2">4.2</option>
              <option value="3">3</option>
            </select>
          </div>
          <Alert tone="info">The export directory must be writable by uid and gid 65532, which the appliance writes as.</Alert>
        </>
      )}

      {text('schedule', 'Schedule', { error: errors.schedule,
        hint: 'Five cron fields: minute, hour, day of month, month and day of week. 0 2 * * * runs every day at 02:00.' })}
      {text('timeZone', 'Time zone', { error: errors.timeZone, list: zones, hint: 'The time zone of the schedule, such as Europe/Prague.' })}
      <datalist id={zones}>
        {Intl.supportedValuesOf('timeZone').map((zone) => <option key={zone} value={zone} />)}
      </datalist>
      {text('retention', 'Backups kept', { inputMode: 'numeric', error: errors.retention, hint: 'How many backups the target keeps; older ones are deleted.' })}

      <div className="flex flex-col gap-2 sm:flex-row">
        <Button type="submit" busy={busy} disabled={!values.name.trim() || needsHostKey}>{editing ? 'Save the target' : 'Add the target'}</Button>
        {editing && <Button variant="secondary" onClick={onCancel}>Cancel</Button>}
      </div>
    </form>
  );
}
