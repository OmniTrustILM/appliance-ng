import { useId, type InputHTMLAttributes } from 'react';
import { cx } from './cx';

export const INPUT = cx(
  'h-10 w-full rounded-md border border-outline bg-surface-raised px-3 text-content placeholder:text-content-subtle transition-[border-color,box-shadow]',
  'hover:border-content-subtle focus:border-brand focus:shadow-[0_0_0_3px_var(--color-brand-ring)] focus:outline-hidden',
  'aria-invalid:border-danger aria-invalid:focus:shadow-[0_0_0_3px_color-mix(in_srgb,var(--color-danger)_20%,transparent)]',
);

type FieldProps = Readonly<
  InputHTMLAttributes<HTMLInputElement> & {
    label: string;
    name: string;
    error?: string;
    hint?: string;
    variant?: 'text' | 'code';
  }
>;

/** A labelled input with an optional hint, showing its validation error below it. */
export function Field({ label, error, hint, variant = 'text', className, ...input }: FieldProps) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy = [hint && hintId, error && errorId].filter(Boolean).join(' ') || undefined;
  return (
    <div className="mb-[18px]">
      <label htmlFor={id} className="mb-1.5 block text-sm font-medium">
        {label}
      </label>
      <input
        id={id}
        {...input}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        className={cx(
          INPUT,
          variant === 'code' && 'font-mono text-lg uppercase tracking-[0.3em]',
          className,
        )}
      />
      {hint && (
        <p id={hintId} className="mt-1 text-[13px] text-content-subtle">
          {hint}
        </p>
      )}
      {error && (
        <p id={errorId} className="mt-1.5 flex items-start gap-1.5 text-[13px] text-danger" role="alert">
          <span
            aria-hidden="true"
            className="mt-px grid size-4 flex-none place-items-center rounded-full bg-danger text-[11px] font-bold text-surface-raised"
          >
            !
          </span>
          {error}
        </p>
      )}
    </div>
  );
}
