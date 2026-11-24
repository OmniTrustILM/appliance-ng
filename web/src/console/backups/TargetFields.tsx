import { useId } from 'react';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { ChoiceField } from '../../components/ChoiceField';
import { Field, INPUT } from '../../components/Field';
import { TextArea } from '../../components/TextArea';
import { HostKeyStep } from './HostKeyStep';
import type { Kind, NFSVersion, SetValue, TextKey, Values } from './targetValues';

type SecretsSet = components['schemas']['BackupSecretsSet'];

const keepHint = 'Leave blank to keep the stored value.';
export const enterAgainHint = 'Enter it again: stored secrets are used only for the server they were given for.';

const kinds: ReadonlyArray<{ value: Kind; label: string; description: string }> = [
  { value: 's3', label: 'S3', description: 'A bucket of an S3 compatible object store.' },
  { value: 'sftp', label: 'SFTP', description: 'A directory on an SSH server.' },
  { value: 'nfs', label: 'NFS', description: 'A directory on an NFS export.' },
];

/** A text field of a target value. */
export function textField(values: Values, set: SetValue) {
  return (key: TextKey, label: string, extra: Partial<Parameters<typeof Field>[0]> = {}) => (
    <Field label={label} name={key} value={values[key]} onChange={(event) => set(key, event.target.value as never)}
      autoComplete="off" spellCheck={false} {...extra} />
  );
}

type TargetFieldsProps = Readonly<{
  values: Values;
  set: SetValue;
  errors: Record<string, string>;
  /** The secrets stored for the edited target. */
  secrets?: SecretsSet;
  /** Whether the edited target's credentials would go to another server. */
  moved?: boolean;
}>;

/** The type of a target, where it is and its credentials; secrets are never shown. */
export function TargetFields({ values, set, errors, secrets, moved = false }: TargetFieldsProps) {
  const versionId = useId();
  const text = textField(values, set);
  const secretHint = (stored: boolean | undefined, otherwise?: string) => {
    if (!stored) return otherwise;
    return moved ? enterAgainHint : keepHint;
  };

  return (
    <>
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
            <label htmlFor={versionId} className="mb-1.5 block text-sm font-medium">NFS version</label>
            <select id={versionId} name="version" value={values.version} className={INPUT}
              onChange={(event) => set('version', event.target.value as NFSVersion)}>
              <option value="4.1">4.1</option>
              <option value="4.2">4.2</option>
              <option value="3">3</option>
            </select>
          </div>
          <Alert tone="info">The export directory must be writable by uid and gid 65532, which the appliance writes as.</Alert>
        </>
      )}
    </>
  );
}
