import { useState } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';
import { SummaryRow } from '../components/SummaryRow';
import { attempt, failureMessage, unreachable } from '../setup/failure';
import { toRequest, type Answers, type CatalogueItem } from './answers';

/** Shows the answers and sends them; the admin can still come back and change them until the setup is complete. */
export function ReviewPage({ answers, catalogue, onBack, onSent, onRefused, onComplete }: Readonly<{
  answers: Answers;
  catalogue: CatalogueItem[];
  onBack: () => void;
  onSent: () => void;
  onRefused: (field: string, message: string) => void;
  onComplete: () => void;
}>) {
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
    <Card
      back={<Button variant="ghost" onClick={onBack} disabled={busy}>Back</Button>}
      actions={<Button onClick={install} busy={busy}>Install</Button>}
    >
      <PageHeading eyebrow="Platform" title="Review" />
      <dl className="m-0">
        <SummaryRow term="ILM host name">{answers.hostName}</SummaryRow>
        <SummaryRow term="Certificate">Built-in certificate authority</SummaryRow>
        <SummaryRow term="Components">{titles.length > 0 ? titles.join(', ') : 'None'}</SummaryRow>
        <SummaryRow term="Mail">{mail ? `${mail.host}:${mail.port} (${mail.security}), from ${mail.from}` : 'No mail'}</SummaryRow>
      </dl>
      {error && <div className="mt-4"><Alert tone="error">{error}</Alert></div>}
    </Card>
  );
}
