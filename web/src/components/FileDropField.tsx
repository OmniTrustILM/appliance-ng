import { useId, useState, type DragEvent } from 'react';
import { cx } from './cx';

type FileDropFieldProps = Readonly<{
  label: string;
  hint?: string;
  required?: boolean;
  file: File | undefined;
  onChange: (file: File | undefined) => void;
}>;

/**
 * A file input with a drop zone: the zone shows the chosen file name and accepts a dropped file, while the
 * native input inside it stays the keyboard and screen reader way to choose one.
 */
export function FileDropField({ label, hint, required = false, file, onChange }: FileDropFieldProps) {
  const hintId = useId();
  const [over, setOver] = useState(false);

  function onDrop(event: DragEvent) {
    event.preventDefault();
    setOver(false);
    const dropped = event.dataTransfer.files[0];
    if (dropped) onChange(dropped);
  }

  return (
    <div className="mb-[18px]">
      <label
        data-testid="drop-zone"
        onDragOver={(event) => {
          event.preventDefault();
          setOver(true);
        }}
        onDragLeave={() => setOver(false)}
        onDrop={onDrop}
        className={cx(
          'block cursor-pointer rounded-md border border-dashed px-3.5 py-3 text-sm transition-colors',
          'focus-within:border-brand focus-within:ring-2 focus-within:ring-brand-ring',
          over ? 'border-brand bg-brand-subtle' : 'border-outline bg-surface-raised hover:border-content-subtle',
        )}
      >
        <span className="block font-medium">{label}{required ? '' : ' (optional)'}</span>
        <span className="block text-content-muted">
          {file ? file.name : 'Drop the file here or choose it.'}
        </span>
        <input
          type="file"
          className="sr-only"
          required={required}
          aria-describedby={hint ? hintId : undefined}
          onChange={(event) => onChange(event.target.files?.[0])}
        />
      </label>
      {hint && <p id={hintId} className="mt-1 text-[13px] text-content-subtle">{hint}</p>}
    </div>
  );
}
