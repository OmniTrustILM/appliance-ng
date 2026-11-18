import { useEffect } from 'react';
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
    <div>
      <h1>Installing the platform</h1>
      {error && <p className="form-error" role="alert">{error}</p>}
      {status?.addonStep && <p>Installing add-on: {status.addonStep}</p>}
      {status && conditions.length === 0 && <p>The platform has not reported anything yet.</p>}
      <ConditionList conditions={conditions} />
      {certificateIssued && <p><RootCaLink /></p>}
      <p>This page refreshes every five seconds. You can change the answers until the setup is complete.</p>
      <button type="button" onClick={onBack}>Change the answers</button>
    </div>
  );
}
