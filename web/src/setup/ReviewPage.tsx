import { useState } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';
import { SummaryRow } from '../components/SummaryRow';
import { attempt, failureMessage, unreachable } from './failure';
import type { NodeSettings } from './NodePage';

export type Answers = { user: string; node: NodeSettings };

/** Summarises the answers and applies them; the password and the authenticator secret are never shown. */
export function ReviewPage({ answers, onBack, onDone, onExpired }: Readonly<{
  answers?: Answers;
  onBack: () => void;
  onDone: () => void;
  onExpired: () => void;
}>) {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function apply() {
    setBusy(true);
    setError(undefined);
    const result = await attempt(() => api.POST('/setup/apply'));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) return onDone();
    if (result.response.status === 401) return onExpired();
    setError(failureMessage(result.response.status));
  }

  const node = answers?.node;
  return (
    <Card
      back={<Button variant="secondary" onClick={onBack}>Change the settings</Button>}
      actions={<Button busy={busy} onClick={apply}>Apply</Button>}
    >
      <PageHeading eyebrow="Setup" title="Review" lead="Check the settings. The node installs and reboots when you apply them." />
      {answers && node ? (
        <dl className="m-0">
          <SummaryRow term="Administrator">{answers.user}</SummaryRow>
          <SummaryRow term="Host name">{node.hostName}</SummaryRow>
          <SummaryRow term="Address" mono={Boolean(node.address)}>{node.address || 'Address from DHCP'}</SummaryRow>
          {node.gateway && <SummaryRow term="Gateway" mono>{node.gateway}</SummaryRow>}
          <SummaryRow term="DNS servers" mono>{node.dns.join(', ')}</SummaryRow>
          {node.ntp.length > 0 && <SummaryRow term="NTP servers" mono>{node.ntp.join(', ')}</SummaryRow>}
          {node.proxy?.http && <SummaryRow term="HTTP proxy" mono>{node.proxy.http}</SummaryRow>}
          {node.proxy?.https && <SummaryRow term="HTTPS proxy" mono>{node.proxy.https}</SummaryRow>}
          {node.proxy?.noProxy && <SummaryRow term="No proxy" mono>{node.proxy.noProxy}</SummaryRow>}
        </dl>
      ) : (
        <p>Your answers were saved before the page was reloaded. Change the settings to see them again.</p>
      )}
      {error && <div className="mt-4"><Alert tone="error">{error}</Alert></div>}
    </Card>
  );
}
