import { useState, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Field } from '../../components/Field';
import { RoleField, type RoleChoice } from './RoleField';
import { useIssue } from './useIssue';

type Role = 'os:reader' | 'os:admin';

const roles: readonly RoleChoice<Role>[] = [
  { value: 'os:reader', label: 'Reader', description: 'Reads the node state, services and logs; no changes.' },
  { value: 'os:admin', label: 'Admin', description: 'Full control of the node, including reboot, reset and upgrade.' },
];

/** Issues a talosconfig for 1 to 8 hours and downloads it. */
export function TalosconfigForm({ onIssued }: Readonly<{ onIssued: (filename: string) => void }>) {
  const [role, setRole] = useState<Role>('os:reader');
  const [hours, setHours] = useState('1');
  const { busy, error, fieldErrors, issue } = useIssue('talosconfig.yaml', onIssued);

  function submit(event: SubmitEvent) {
    event.preventDefault();
    issue(() => api.POST('/access/talosconfig', { body: { role, hours: Number(hours) }, parseAs: 'blob' }));
  }

  return (
    <form onSubmit={submit} noValidate>
      {error && <Alert tone="error">{error}</Alert>}
      <RoleField name="talos-role" roles={roles} value={role} onChange={setRole} error={fieldErrors.role} />
      <Field label="Hours" name="talos-hours" type="number" min={1} max={8} value={hours}
        onChange={(event) => setHours(event.target.value)} error={fieldErrors.hours} />
      <Button type="submit" busy={busy}>Issue the talosconfig</Button>
    </form>
  );
}
