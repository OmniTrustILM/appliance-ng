import { useState, type SubmitEvent } from 'react';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ChoiceField } from '../components/ChoiceField';
import { MailFields } from '../components/MailFields';
import { PageHeading } from '../components/PageHeading';
import { emptyMail, mailProblems, type Answers, type Errors, type MailAnswers } from './answers';

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
        {mail && <MailFields mail={mail} passwordSet={passwordSet} errors={shown} onEdit={edit} />}
      </Card>
    </form>
  );
}
