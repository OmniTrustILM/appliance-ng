import { useState, type SubmitEvent } from 'react';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ChoiceField } from '../components/ChoiceField';
import { Field, INPUT } from '../components/Field';
import { PageHeading } from '../components/PageHeading';
import { emptyMail, mailProblems, type Answers, type Errors, type MailAnswers, type MailSecurity } from './answers';

const securities: { value: MailSecurity; label: string }[] = [
  { value: 'none', label: 'None' },
  { value: 'starttls', label: 'STARTTLS' },
  { value: 'tls', label: 'TLS' },
];

/** Asks for the mail server of the ILM; mail can be skipped. */
export function MailPage({ answers, passwordSet, errors, onChange, onNext, onBack }: Readonly<{
  answers: Answers;
  passwordSet: boolean;
  errors: Errors;
  onChange: (patch: Partial<Answers>) => void;
  onNext: () => void;
  onBack: () => void;
}>) {
  const [problems, setProblems] = useState<Errors>({});
  const { mail } = answers;
  const shown = { ...errors, ...problems };

  function edit(patch: Partial<MailAnswers>) {
    setProblems({});
    onChange({ mail: { ...(mail ?? emptyMail), ...patch } });
  }

  function submit(event: SubmitEvent) {
    event.preventDefault();
    const found = mail ? mailProblems(mail) : {};
    setProblems(found);
    if (Object.keys(found).length === 0) onNext();
  }

  return (
    <form onSubmit={submit} noValidate>
      <Card
        back={<Button variant="ghost" onClick={onBack}>Back</Button>}
        actions={<Button type="submit">Continue</Button>}
      >
        <PageHeading eyebrow="Platform" title="Mail" />
        <ChoiceField
          type="checkbox"
          label="Send mail"
          checked={mail !== undefined}
          onChange={(e) => onChange({ mail: e.target.checked ? emptyMail : undefined })}
        />
        <p className="mb-[18px] text-content-muted">The ILM sends notifications through this server. Skip it to set mail up later.</p>
        {mail && (
          <>
            <div className="grid gap-x-4 sm:grid-cols-[1fr_140px]">
              <Field label="Mail server" name="mail.host" value={mail.host} onChange={(e) => edit({ host: e.target.value })} error={shown['mail.host']} />
              <Field label="Port" name="mail.port" inputMode="numeric" value={mail.port} onChange={(e) => edit({ port: e.target.value })} error={shown['mail.port']} />
            </div>
            <div className="mb-[18px]">
              <label htmlFor="mail-security" className="mb-1.5 block text-sm font-medium">Security</label>
              <select id="mail-security" name="mail.security" className={INPUT} value={mail.security} onChange={(e) => edit({ security: e.target.value as MailSecurity })}>
                {securities.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
              </select>
              {shown['mail.security'] && <p className="mt-1.5 text-[13px] text-danger" role="alert">{shown['mail.security']}</p>}
            </div>
            <Field label="From address" name="mail.from" value={mail.from} onChange={(e) => edit({ from: e.target.value })} error={shown['mail.from']} autoComplete="off" />
            <Field label="User name (optional)" name="mail.username" value={mail.username} onChange={(e) => edit({ username: e.target.value })} error={shown['mail.username']} autoComplete="off" />
            <Field
              label="Password (optional)"
              name="mail.password"
              type="password"
              value={mail.password}
              onChange={(e) => edit({ password: e.target.value })}
              error={shown['mail.password']}
              hint={passwordSet ? 'A password is set. Leave the field empty to keep it.' : undefined}
              autoComplete="new-password"
            />
          </>
        )}
      </Card>
    </form>
  );
}
