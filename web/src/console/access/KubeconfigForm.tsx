import { useState, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { ChoiceField } from '../../components/ChoiceField';
import { Field } from '../../components/Field';
import { RoleField, type RoleChoice } from './RoleField';
import { useIssue } from './useIssue';

type Role = 'view' | 'cluster-admin';

const roles: readonly RoleChoice<Role>[] = [
  { value: 'view', label: 'View', description: 'Reads most objects; no Secrets and no changes.' },
  { value: 'cluster-admin', label: 'Cluster admin', description: 'Full control of the cluster.' },
];

/** Issues a kubeconfig, time-limited or long-lived with a name, and downloads it. */
export function KubeconfigForm({ onIssued }: Readonly<{ onIssued: (filename: string) => void }>) {
  const [role, setRole] = useState<Role>('view');
  const [longLived, setLongLived] = useState(false);
  const [hours, setHours] = useState('8');
  const [name, setName] = useState('');
  const { busy, error, fieldErrors, issue } = useIssue('kubeconfig.yaml', onIssued);

  function submit(event: SubmitEvent) {
    event.preventDefault();
    const body = longLived ? { role, name: name.trim() } : { role, hours: Number(hours) };
    issue(() => api.POST('/access/kubeconfig', { body, parseAs: 'blob' }));
  }

  return (
    <form onSubmit={submit} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <RoleField name="kube-role" roles={roles} value={role} onChange={setRole} error={fieldErrors.role} />
      <fieldset className="mb-[18px]">
        <legend className="mb-1.5 text-sm font-medium">Validity</legend>
        <ChoiceField type="radio" name="kube-validity" label="Time-limited" description="The token expires after 1 to 24 hours."
          checked={!longLived} onChange={() => setLongLived(false)} />
        <ChoiceField type="radio" name="kube-validity" label="Long-lived" description="Valid until it is revoked; it needs a name."
          checked={longLived} onChange={() => setLongLived(true)} />
      </fieldset>
      {longLived ? (
        <Field label="Name" name="name" value={name} onChange={(event) => setName(event.target.value)} error={fieldErrors.name}
          autoComplete="off" spellCheck={false} hint="Up to 40 lower-case letters, digits and hyphens, such as ci-pipeline." />
      ) : (
        <Field label="Hours" name="hours" type="number" min={1} max={24} value={hours}
          onChange={(event) => setHours(event.target.value)} error={fieldErrors.hours} />
      )}
      <Button type="submit" busy={busy} disabled={longLived && !name.trim()}>Issue the kubeconfig</Button>
    </form>
  );
}
