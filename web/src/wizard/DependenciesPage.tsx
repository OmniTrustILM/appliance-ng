import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ChoiceField } from '../components/ChoiceField';
import { PageHeading } from '../components/PageHeading';
import { DependencyChoices } from '../dependencies/DependencyChoices';
import type { Answers, Errors } from './answers';

/** Chooses, for the database, messaging and identity provider, managed or external, and utils. */
export function DependenciesPage({ answers, errors, onChange, onNext, onBack }: Readonly<{
  answers: Answers;
  errors: Errors;
  onChange: (patch: Partial<Answers>) => void;
  onNext: () => void;
  onBack: () => void;
}>) {
  return (
    <Card
      back={<Button variant="ghost" onClick={onBack}>Back</Button>}
      actions={<Button onClick={onNext}>Continue</Button>}
    >
      <PageHeading eyebrow="Platform" title="Dependencies"
        lead="The appliance runs what ILM needs. Choose external services you run instead; this choice is fixed once the setup is complete." />
      <DependencyChoices answers={answers.dependencies} errors={errors} onChange={(dependencies) => onChange({ dependencies })} />
      <ChoiceField type="checkbox" label="Utils service" description="Installs the utils service with the ILM."
        checked={answers.utils} onChange={(e) => onChange({ utils: e.target.checked })} />
    </Card>
  );
}
