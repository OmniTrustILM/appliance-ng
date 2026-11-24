import { useId, type ReactNode } from 'react';
import { INPUT } from './Field';
import { cx } from './cx';

type TextAreaProps = Readonly<{ label: string; name: string; value: string; onChange: (value: string) => void; hint?: ReactNode; error?: string }>;

/** A labelled multi-line input for PEM text, showing its validation error below it. */
export function TextArea({ label, name, value, onChange, hint, error }: TextAreaProps) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy = [hint && hintId, error && errorId].filter(Boolean).join(' ') || undefined;
  return (
    <div className="mb-[18px]">
      <label htmlFor={id} className="mb-1.5 block text-sm font-medium">{label}</label>
      <textarea id={id} name={name} value={value} onChange={(event) => onChange(event.target.value)} rows={4}
        spellCheck={false} autoComplete="off" aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        className={cx(INPUT, 'h-auto py-2 font-mono text-[13px]')} />
      {hint && <p id={hintId} className="mt-1 text-[13px] text-content-subtle">{hint}</p>}
      {error && <p id={errorId} className="mt-1.5 text-[13px] text-danger" role="alert">{error}</p>}
    </div>
  );
}
