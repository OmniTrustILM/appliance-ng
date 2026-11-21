import { useCallback, useState, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { MailFields } from '../../components/MailFields';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, unreachable } from '../../setup/failure';
import {
  emptyMail, keepsStoredPassword, mailAnswersOf, mailProblems, mailRequest, type Errors, type MailAnswers,
} from '../../wizard/answers';
import { useLoaded } from '../shared/useLoaded';
import { MailTest } from './MailTest';

const saved = 'The change is saved. The platform picks it up within a minute.';
const mailFields = new Set(['host', 'port', 'security', 'from', 'username', 'password']);

const loadMail = () => api.GET('/mail');

const same = (a: MailAnswers, b: MailAnswers) => JSON.stringify(a) === JSON.stringify(b);

/** Sets, changes or removes the mail server of the platform and tests it; the password is never shown. */
export function MailPage() {
  const { data, error: loadError, reload } = useLoaded(loadMail);
  const stored = data?.mail && mailAnswersOf(data.mail);
  const [form, setForm] = useState<MailAnswers>(emptyMail);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState(false);

  // A fresh read replaces the form, which also forgets the typed password.
  const [seen, setSeen] = useState(data);
  if (data !== seen) {
    setSeen(data);
    setForm(stored ?? emptyMail);
  }

  const edit = useCallback((patch: Partial<MailAnswers>) => {
    setErrors({});
    setNotice(undefined);
    setForm((current) => ({ ...current, ...patch }));
  }, []);

  async function save() {
    const found = mailProblems(form);
    setErrors(found);
    if (Object.keys(found).length > 0) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    const result = await attempt(() => api.PUT('/mail', { body: mailRequest(form) }));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) {
      setNotice(saved);
      return reload();
    }
    const failure = result.error;
    const message = failure?.message ?? 'The appliance could not process the request.';
    if (failure?.field && mailFields.has(failure.field)) return setErrors({ [`mail.${failure.field}`]: message });
    setError(message);
  }

  async function remove() {
    setRemoving(false);
    setError(undefined);
    setNotice(undefined);
    const result = await attempt(() => api.DELETE('/mail'));
    if (!result) return setError(unreachable);
    if (!result.response.ok) return setError(result.error?.message ?? 'The appliance could not remove the mail server.');
    setNotice('The mail server is removed.');
    reload();
  }

  if (!data) {
    return (
      <PageWidth>
        <Card>{loadError ? <Alert tone="error">{loadError}</Alert> : <Loading />}</Card>
      </PageWidth>
    );
  }

  const unsaved = !stored || !same(stored, form);

  return (
    <PageWidth>
      <form onSubmit={(event: SubmitEvent) => { event.preventDefault(); void save(); }} noValidate>
        <Card
          back={stored && <Button variant="secondary" onClick={() => setRemoving(true)}>Remove the mail server</Button>}
          actions={<Button type="submit" busy={busy}>Save</Button>}
        >
          <PageHeading eyebrow="Platform" title="Mail" lead="The ILM sends notifications through this server." />
          {notice && <Alert tone="success">{notice}</Alert>}
          {error && <Alert tone="error">{error}</Alert>}
          <MailFields
            mail={form}
            passwordSet={Boolean(data.mail?.passwordSet) && keepsStoredPassword(stored, form)}
            errors={errors}
            onEdit={edit}
          />
        </Card>
      </form>

      <div className="mt-6">
        <Card>
          <MailTest disabled={unsaved} settings={data} />
        </Card>
      </div>

      {removing && (
        <ConfirmDialog title="Remove the mail server?" confirmLabel="Remove" onConfirm={() => void remove()} onCancel={() => setRemoving(false)}>
          The mail server and its credentials are deleted. The ILM stops sending notifications.
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
