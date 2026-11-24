import { useId, useState, type SubmitEvent } from 'react';
import { api, timeoutSignal } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { attempt, unreachable } from '../../setup/failure';
import { TargetFields, textField } from './TargetFields';
import { updateOf, useTargetValues, type Target, type Values } from './targetValues';

export type { Target } from './targetValues';

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

type TargetFormProps = Readonly<{
  editing?: Target;
  onSaved: () => void;
  onCancel: () => void;
}>;

/** Adds a backup target or, when `editing`, changes one; secrets are never shown and left blank keep the stored ones. */
export function TargetForm({ editing, onSaved, onCancel }: TargetFormProps) {
  const [values, set] = useTargetValues(editing);
  const [busy, setBusy] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<string>();
  const zones = useId();
  // Stored credentials stay with their server: sent elsewhere, they must be entered again.
  const moved = editing !== undefined && relocated(editing, values);
  const text = textField(values, set);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    setErrors({});
    const result = await attempt(() => api.PUT('/backups/targets/{name}', {
      params: { path: { name: values.name.trim() } },
      body: updateOf(values),
      signal: timeoutSignal(),
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
      <TargetFields values={values} set={set} errors={errors} secrets={editing?.secretsSet} moved={moved} />

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
