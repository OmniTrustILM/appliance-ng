import type { InputHTMLAttributes, ReactNode } from 'react';
import { cx } from './cx';

type ChoiceFieldProps = Readonly<
  Omit<InputHTMLAttributes<HTMLInputElement>, 'type'> & {
    type: 'radio' | 'checkbox';
    label: string;
    description?: ReactNode;
    badge?: ReactNode;
  }
>;

/** A bordered radio or checkbox row with a title, optional description and badge. */
export function ChoiceField({ label, description, badge, className, ...input }: ChoiceFieldProps) {
  return (
    <label
      className={cx(
        'mb-2 flex items-start gap-3 rounded-md border px-3.5 py-3',
        input.disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer',
        !input.disabled && 'not-has-[:checked]:hover:bg-surface-sunken',
        'border-border has-[:checked]:border-brand has-[:checked]:bg-brand-subtle',
        className,
      )}
    >
      <input {...input} className="mt-[3px] size-4 accent-brand" />
      <span className="flex min-w-0 flex-1 flex-wrap items-start justify-between gap-x-3 gap-y-1.5">
        <span className="min-w-0 flex-1 basis-48">
          <span className="block font-medium">{label}</span>
          {description && <span className="block text-[13px] text-content-muted">{description}</span>}
        </span>
        {badge}
      </span>
    </label>
  );
}
