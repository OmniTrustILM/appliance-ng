import type { ReactNode } from 'react';
import { ChoiceField } from './ChoiceField';

export type ComponentChoice = {
  name: string;
  title: string;
  description?: ReactNode;
  disabled?: boolean;
  badge?: ReactNode;
};

const legend = 'Install these components with the ILM';

/** The chosen components after `name` is checked or unchecked. */
export function toggled(chosen: readonly string[], name: string, checked: boolean): string[] {
  const others = chosen.filter((c) => c !== name);
  return checked ? [...others, name] : others;
}

/** A checkbox row per component; a disabled one cannot be chosen yet. */
export function ComponentChoices({ choices, chosen, onToggle }: Readonly<{
  choices: readonly ComponentChoice[];
  chosen: readonly string[];
  onToggle: (name: string, checked: boolean) => void;
}>) {
  return (
    <fieldset className="mb-[18px]">
      <legend className="sr-only">{legend}</legend>
      {choices.map(({ name, title, description, disabled, badge }) => (
        <ChoiceField
          key={name}
          type="checkbox"
          label={title}
          description={description}
          checked={chosen.includes(name)}
          disabled={disabled}
          onChange={(e) => onToggle(name, e.target.checked)}
          badge={badge}
        />
      ))}
    </fieldset>
  );
}
