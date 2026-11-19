import { Alert } from '../components/Alert';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ComponentChoices, toggled } from '../components/ComponentChoices';
import { PageHeading } from '../components/PageHeading';
import type { Answers, CatalogueItem } from './answers';

/** Offers the components of the catalogue; those that need registry credentials cannot be chosen yet. */
export function ComponentsPage({ catalogue, answers, error, onChange, onNext, onBack }: Readonly<{
  catalogue: CatalogueItem[];
  answers: Answers;
  error?: string;
  onChange: (patch: Partial<Answers>) => void;
  onNext: () => void;
  onBack: () => void;
}>) {
  const choices = catalogue.map(({ name, title, description, private: isPrivate }) => ({
    name,
    title,
    description,
    disabled: isPrivate,
    badge: isPrivate && <Badge tone="muted">needs registry credentials, available later</Badge>,
  }));

  return (
    <Card
      back={<Button variant="ghost" onClick={onBack}>Back</Button>}
      actions={<Button onClick={onNext}>Continue</Button>}
    >
      <PageHeading eyebrow="Platform" title="Components" lead="Install these components with the ILM. You can add more later." />
      <ComponentChoices
        choices={choices}
        chosen={answers.components}
        onToggle={(name, checked) => onChange({ components: toggled(answers.components, name, checked) })}
      />
      {error && <Alert tone="error">{error}</Alert>}
    </Card>
  );
}
