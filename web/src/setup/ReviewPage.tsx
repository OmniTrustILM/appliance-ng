import { useState } from 'react';
import { api } from '../api/client';
import { attempt, failureMessage, unreachable } from './failure';
import type { NodeSettings } from './NodePage';

export type Answers = { user: string; node: NodeSettings };

function Row({ term, children }: Readonly<{ term: string; children: string }>) {
  return (
    <>
      <dt>{term}</dt>
      <dd>{children}</dd>
    </>
  );
}

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
    const result = await attempt(() => api.POST('/api/v1/setup/apply'));
    setBusy(false);
    if (!result) return setError(unreachable);
    if (result.response.ok) return onDone();
    if (result.response.status === 401) return onExpired();
    setError(failureMessage(result.response.status));
  }

  const node = answers?.node;
  return (
    <div>
      <h1>Review</h1>
      {answers && node ? (
        <dl>
          <Row term="Administrator">{answers.user}</Row>
          <Row term="Host name">{node.hostName}</Row>
          <Row term="Address">{node.address || 'Address from DHCP'}</Row>
          {node.gateway && <Row term="Gateway">{node.gateway}</Row>}
          <Row term="DNS servers">{node.dns.join(', ')}</Row>
          {node.ntp.length > 0 && <Row term="NTP servers">{node.ntp.join(', ')}</Row>}
          {node.proxy?.http && <Row term="HTTP proxy">{node.proxy.http}</Row>}
          {node.proxy?.https && <Row term="HTTPS proxy">{node.proxy.https}</Row>}
          {node.proxy?.noProxy && <Row term="No proxy">{node.proxy.noProxy}</Row>}
        </dl>
      ) : (
        <p>Your answers were saved before the page was reloaded. Change the settings to see them again.</p>
      )}
      {error && <p className="form-error" role="alert">{error}</p>}
      <button type="button" onClick={onBack}>Change the settings</button>
      <button type="button" onClick={apply} disabled={busy}>Apply</button>
    </div>
  );
}
