import { useEffect } from 'react';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';
import { ConditionList } from '../console/status/ConditionList';
import { useStatus } from '../console/status/useStatus';
import { RootCaLink } from './RootCaLink';

/** Follows the installation of the platform; it finishes by itself when the setup is complete. */
export function ProgressPage({ onBack, onComplete }: Readonly<{ onBack: () => void; onComplete: () => void }>) {
  const { status, error } = useStatus();
  const complete = status?.setup === 'complete';
  const conditions = status?.conditions ?? [];
  const certificateIssued = conditions.some(({ type, status: state }) => type === 'CertificateReady' && state === 'True');

  useEffect(() => {
    if (complete) onComplete();
  }, [complete, onComplete]);

  return (
    <Card back={<Button variant="secondary" onClick={onBack}>Change the answers</Button>}>
      <PageHeading
        eyebrow="Platform"
        title="Installing the platform"
        lead="This page refreshes every five seconds. You can change the answers until the setup is complete."
      />
      {error && <Alert tone="error">{error}</Alert>}
      {status?.addonStep && <Alert tone="info" busy>Installing add-on: {status.addonStep}</Alert>}
      {status && conditions.length === 0 && <p className="text-content-muted">The platform has not reported anything yet.</p>}
      <ConditionList conditions={conditions} markers />
      {certificateIssued && <p className="mt-4"><RootCaLink /></p>}
    </Card>
  );
}
