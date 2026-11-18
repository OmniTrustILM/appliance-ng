import { useState } from 'react';
import { api } from '../api/client';
import { attempt, failureMessage, unreachable } from '../setup/failure';
import { toRequest, type Answers, type CatalogueItem } from './answers';

/** Shows the answers and sends them; the admin can still come back and change them until the setup is complete. */
export function ReviewPage({ answers, catalogue, onBack, onSent, onRefused, onComplete }: {
  answers: Answers;
  catalogue: CatalogueItem[];
  onBack: () => void;
  onSent: () => void;
  onRefused: (field: string, message: string) => void;
  onComplete: () => void;
}) {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const titles = answers.components.map((name) => catalogue.find((item) => item.name === name)?.title ?? name);
  const { mail } = answers;

  async function install() {
    setBusy(true);
    setError(undefined);
    const result = await attempt(() => api.PUT('/api/v1/wizard/platform', { body: toRequest(answers) }));
    setBusy(false);
    if (!result) return setError(unreachable);
    const { response, error: body } = result;
    if (response.ok) return onSent();
    if (response.status === 400 && body && 'field' in body && body.field) return onRefused(body.field, body.message);
    if (response.status === 409 && body && 'code' in body && body.code === 'setup_complete') return onComplete();
    setError(failureMessage(response.status));
  }

  return (
    <div>
      <h1>Review</h1>
      <dl>
        <dt>ILM host name</dt>
        <dd>{answers.hostName}</dd>
        <dt>Certificate</dt>
        <dd>Built-in certificate authority</dd>
        <dt>Components</dt>
        <dd>{titles.length > 0 ? titles.join(', ') : 'None'}</dd>
        <dt>Mail</dt>
        <dd>{mail ? `${mail.host}:${mail.port} (${mail.security}), from ${mail.from}` : 'No mail'}</dd>
      </dl>
      {error && <p className="form-error" role="alert">{error}</p>}
      <button type="button" onClick={onBack} disabled={busy}>Back</button>
      <button type="button" onClick={install} disabled={busy}>Install</button>
    </div>
  );
}
