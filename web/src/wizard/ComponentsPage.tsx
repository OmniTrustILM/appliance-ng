import { Alert } from '../components/Alert';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ChoiceField } from '../components/ChoiceField';
import { PageHeading } from '../components/PageHeading';
import type { Answers, CatalogueItem } from './answers';

const legend = 'Install these components with the ILM';

/** Offers the components of the catalogue; those that need registry credentials cannot be chosen yet. */
export function ComponentsPage({ catalogue, answers, error, onChange, onNext, onBack }: Readonly<{
  catalogue: CatalogueItem[];
  answers: Answers;
  error?: string;
  onChange: (patch: Partial<Answers>) => void;
  onNext: () => void;
  onBack: () => void;
}>) {
  function toggle(name: string, checked: boolean) {
    const others = answers.components.filter((c) => c !== name);
    onChange({ components: checked ? [...others, name] : others });
  }

  return (
    <Card
      back={<Button variant="ghost" onClick={onBack}>Back</Button>}
      actions={<Button onClick={onNext}>Continue</Button>}
    >
      <PageHeading eyebrow="Platform" title="Components" lead={`${legend}. You can add more later.`} />
      <fieldset className="mb-[18px]">
        <legend className="sr-only">{legend}</legend>
        {catalogue.map(({ name, title, description, private: isPrivate }) => (
          <ChoiceField
            key={name}
            type="checkbox"
            label={title}
            description={description}
            checked={answers.components.includes(name)}
            disabled={isPrivate}
            onChange={(e) => toggle(name, e.target.checked)}
            badge={isPrivate && <Badge tone="muted">needs registry credentials, available later</Badge>}
          />
        ))}
      </fieldset>
      {error && <Alert tone="error">{error}</Alert>}
    </Card>
  );
}
