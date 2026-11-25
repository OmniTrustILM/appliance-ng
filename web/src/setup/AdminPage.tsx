import { useState, type SubmitEvent } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { Field } from '../components/Field';
import { PageHeading } from '../components/PageHeading';
import { attempt, failureMessage, unreachable } from './failure';
import { adminProblems } from './validate';

type Errors = Partial<Record<string, string>>;

/** Asks for the administrator account of the appliance. */
export function AdminPage({ onBack, onDone, onExpired }: Readonly<{
  onBack: () => void;
  onDone: (user: string) => void;
  onExpired: () => void;
}>) {
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [errors, setErrors] = useState<Errors>({});
  const [busy, setBusy] = useState(false);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const problems = adminProblems({ user, password, confirmation });
    setErrors(problems);
    if (Object.keys(problems).length > 0) return;
    setBusy(true);
    const result = await attempt(() => api.PUT('/setup/admin', { body: { user, password } }));
    setBusy(false);
    if (!result) return setErrors({ form: unreachable });
    const { response, error } = result;
    if (response.ok) return onDone(user);
    if (response.status === 401) return onExpired();
    if (response.status === 400 && error && 'field' in error && error.field) {
      return setErrors({ [error.field]: error.message });
    }
    setErrors({ form: failureMessage(response.status) });
  }

  return (
    <form onSubmit={submit} noValidate>
      <Card
        back={<Button variant="secondary" disabled={busy} onClick={onBack}>Back</Button>}
        actions={<Button type="submit" busy={busy}>Continue</Button>}
      >
        <PageHeading eyebrow="Setup" title="Administrator" lead="The account that signs in to this console." />
        <Field label="User name" name="user" value={user} onChange={(e) => setUser(e.target.value)} error={errors.user} autoComplete="username" />
        <Field label="Password" name="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} error={errors.password} autoComplete="new-password" />
        <Field label="Confirm password" name="confirmation" type="password" value={confirmation} onChange={(e) => setConfirmation(e.target.value)} error={errors.confirmation} autoComplete="new-password" />
        {errors.form && <Alert tone="error">{errors.form}</Alert>}
      </Card>
    </form>
  );
}
