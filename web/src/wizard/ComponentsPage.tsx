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
  function toggle(name: string, checked: boolean) {
    const others = answers.components.filter((c) => c !== name);
    onChange({ components: checked ? [...others, name] : others });
  }

  return (
    <div>
      <h1>Components</h1>
      <fieldset>
        <legend>Install these components with the ILM</legend>
        {catalogue.map(({ name, title, description, private: isPrivate }) => (
          <label key={name}>
            <input
              type="checkbox"
              checked={answers.components.includes(name)}
              disabled={isPrivate}
              onChange={(e) => toggle(name, e.target.checked)}
            />{' '}
            {title}
            {description && <> – {description}</>}
            {isPrivate && <> (needs registry credentials, available later)</>}
          </label>
        ))}
      </fieldset>
      {error && <p className="form-error" role="alert">{error}</p>}
      <button type="button" onClick={onBack}>Back</button>
      <button type="button" onClick={onNext}>Continue</button>
    </div>
  );
}
