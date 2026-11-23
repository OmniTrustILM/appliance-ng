import { useCallback, useEffect, useState } from 'react';
import { api, setCsrfToken } from '../api/client';
import { Alert } from '../components/Alert';
import { PageWidth } from '../components/PageWidth';
import { Loading } from '../components/Spinner';
import { StepIndicator } from '../components/StepIndicator';
import { AdminPage } from './AdminPage';
import { ApplyingPage } from './ApplyingPage';
import { CodePage } from './CodePage';
import { attempt, unreachable } from './failure';
import { ModePage } from './ModePage';
import { NodePage } from './NodePage';
import { ReviewPage, type Answers } from './ReviewPage';
import { TotpPage } from './TotpPage';

type Step = 'code' | 'mode' | 'admin' | 'totp' | 'node' | 'apply' | 'applying';

const steps: { step: Step; label: string }[] = [
  { step: 'code', label: 'Code' },
  { step: 'mode', label: 'Choice' },
  { step: 'admin', label: 'Administrator' },
  { step: 'totp', label: 'Authenticator' },
  { step: 'node', label: 'Node' },
  { step: 'apply', label: 'Review' },
  { step: 'applying', label: 'Installing' },
];

/** Wizard part 1. The server keeps the answers; this page keeps only what the summary needs, in memory. */
export function SetupFlow() {
  const [step, setStep] = useState<Step>();
  const [error, setError] = useState<string>();
  const [user, setUser] = useState<string>();
  const [node, setNode] = useState<Answers['node']>();
  const [address, setAddress] = useState<string>();

  useEffect(() => {
    let active = true;
    void attempt(() => api.GET('/setup/state')).then((result) => {
      if (!active) return;
      if (!result?.data) return setError(unreachable);
      setCsrfToken(result.data.csrfToken);
      setStep(result.data.step);
    });
    return () => {
      active = false;
    };
  }, []);

  const expired = useCallback(() => {
    setCsrfToken(undefined);
    setStep('code');
  }, []);

  if (error) return <PageWidth><Alert tone="error">{error}</Alert></PageWidth>;
  if (!step) return <PageWidth><Loading /></PageWidth>;

  return (
    <PageWidth>
      <StepIndicator steps={steps.map(({ label }) => label)} current={steps.findIndex((s) => s.step === step)} />
      {step === 'code' && <CodePage onDone={() => setStep('mode')} />}
      {step === 'mode' && <ModePage onDone={() => setStep('admin')} onExpired={expired} />}
      {step === 'admin' && <AdminPage onDone={(name) => { setUser(name); setStep('totp'); }} onExpired={expired} />}
      {step === 'totp' && <TotpPage onDone={() => setStep('node')} onExpired={expired} />}
      {step === 'node' && (
        <NodePage
          initial={node}
          onDone={(saved, newAddress) => { setNode(saved); setAddress(newAddress); setStep('apply'); }}
          onExpired={expired}
        />
      )}
      {step === 'apply' && (
        <ReviewPage
          answers={user && node ? { user, node } : undefined}
          onBack={() => setStep('node')}
          onDone={() => setStep('applying')}
          onExpired={expired}
        />
      )}
      {step === 'applying' && <ApplyingPage address={address} />}
    </PageWidth>
  );
}
