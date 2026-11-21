import { useCallback, useEffect, useState } from 'react';
import { Navigate, useNavigate } from 'react-router';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { PageWidth } from '../components/PageWidth';
import { Loading } from '../components/Spinner';
import { StepIndicator } from '../components/StepIndicator';
import { attempt, unreachable } from '../setup/failure';
import { answersOf, keepsStoredPassword, stepOfField, type Answers, type CatalogueItem, type Errors, type MailAnswers, type Step } from './answers';
import { CertificatePage } from './CertificatePage';
import { ComponentsPage } from './ComponentsPage';
import { HostPage } from './HostPage';
import { MailPage } from './MailPage';
import { ProgressPage } from './ProgressPage';
import { ReviewPage } from './ReviewPage';

const steps: { step: Step; label: string }[] = [
  { step: 'host', label: 'Host name' },
  { step: 'certificate', label: 'Certificate' },
  { step: 'components', label: 'Components' },
  { step: 'mail', label: 'Mail' },
  { step: 'review', label: 'Review' },
  { step: 'progress', label: 'Installing' },
];

type Loaded = { answers: Answers; catalogue: CatalogueItem[]; passwordSet: boolean; step: Step };

async function load(): Promise<Loaded | 'complete' | undefined> {
  const [status, appliance, catalogue] = await Promise.all([
    attempt(() => api.GET('/status')),
    attempt(() => api.GET('/appliance')),
    attempt(() => api.GET('/catalogue')),
  ]);
  if (status?.data?.setup === 'complete') return 'complete';
  if (!status?.data || !appliance?.data || !catalogue?.data) return undefined;
  return {
    answers: answersOf(appliance.data),
    catalogue: catalogue.data.items,
    passwordSet: appliance.data.mail?.passwordSet ?? false,
    // Answers were sent before: show how the installation goes; the admin can still change them.
    step: appliance.data.platform ? 'progress' : 'host',
  };
}

/** Wizard part 2. The appliance keeps the answers; this page keeps them in memory while they are edited. */
export function WizardFlow() {
  const [loaded, setLoaded] = useState<Loaded | 'complete'>();
  const [failed, setFailed] = useState(false);
  const [answers, setAnswers] = useState<Answers>();
  const [passwordSet, setPasswordSet] = useState(false);
  // The mail settings the stored password was given for.
  const [storedMail, setStoredMail] = useState<MailAnswers>();
  const [step, setStep] = useState<Step>('host');
  const [errors, setErrors] = useState<Errors>({});
  const navigate = useNavigate();

  useEffect(() => {
    let active = true;
    void load().then((result) => {
      if (!active) return;
      if (!result) return setFailed(true);
      setLoaded(result);
      if (result !== 'complete') {
        setAnswers(result.answers);
        setPasswordSet(result.passwordSet);
        setStoredMail(result.answers.mail);
        setStep(result.step);
      }
    });
    return () => {
      active = false;
    };
  }, []);

  const finished = useCallback(() => navigate('/wizard/done'), [navigate]);

  if (failed) return <PageWidth><Alert tone="error">{unreachable}</Alert></PageWidth>;
  if (loaded === 'complete') return <Navigate to="/" replace />;
  if (!loaded || !answers) return <PageWidth><Loading /></PageWidth>;

  function change(patch: Partial<Answers>) {
    setErrors({});
    setAnswers((current) => current && { ...current, ...patch });
  }

  function sent() {
    // The password was written; the appliance keeps it and this page forgets it.
    setPasswordSet(answers?.mail ? answers.mail.password !== '' || passwordSet : false);
    setStoredMail(answers?.mail);
    setAnswers((current) => current && { ...current, mail: current.mail && { ...current.mail, password: '' } });
    setStep('progress');
  }

  function refused(field: string, message: string) {
    setErrors({ [field]: message });
    setStep(stepOfField(field));
  }

  return (
    <PageWidth>
      <StepIndicator steps={steps.map(({ label }) => label)} current={steps.findIndex((s) => s.step === step)} />
      {step === 'host' && <HostPage answers={answers} errors={errors} onChange={change} onNext={() => setStep('certificate')} />}
      {step === 'certificate' && <CertificatePage onBack={() => setStep('host')} onNext={() => setStep('components')} />}
      {step === 'components' && (
        <ComponentsPage
          catalogue={loaded.catalogue}
          answers={answers}
          error={errors.components}
          onChange={change}
          onBack={() => setStep('certificate')}
          onNext={() => setStep('mail')}
        />
      )}
      {step === 'mail' && (
        <MailPage answers={answers} passwordSet={passwordSet && keepsStoredPassword(storedMail, answers.mail)} errors={errors} onChange={change} onBack={() => setStep('components')} onNext={() => setStep('review')} />
      )}
      {step === 'review' && (
        <ReviewPage
          answers={answers}
          catalogue={loaded.catalogue}
          onBack={() => setStep('mail')}
          onSent={sent}
          onRefused={refused}
          onComplete={() => navigate('/')}
        />
      )}
      {step === 'progress' && <ProgressPage onBack={() => setStep('host')} onComplete={finished} />}
    </PageWidth>
  );
}
