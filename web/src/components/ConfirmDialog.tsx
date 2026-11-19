import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from 'react';
import { Button } from './Button';

type ConfirmDialogProps = Readonly<{
  title: string;
  confirmLabel: string;
  children: ReactNode;
  onConfirm: () => void;
  onCancel: () => void;
}>;

/** A modal confirmation: focus starts on Cancel, Tab stays inside, Escape cancels and focus returns to the opener. */
export function ConfirmDialog({ title, confirmLabel, children, onConfirm, onCancel }: ConfirmDialogProps) {
  const titleId = useId();
  const box = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const opener = document.activeElement;
    box.current?.querySelector('button')?.focus();
    return () => {
      if (opener instanceof HTMLElement) opener.focus();
    };
  }, []);

  // Escape cancels wherever the focus is, also after a click on the dialog's text.
  useEffect(() => {
    const onEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      event.stopPropagation();
      onCancel();
    };
    document.addEventListener('keydown', onEscape);
    return () => document.removeEventListener('keydown', onEscape);
  }, [onCancel]);

  function onKeyDown(event: KeyboardEvent) {
    if (event.key !== 'Tab') return;
    const buttons = box.current?.querySelectorAll('button');
    if (!buttons?.length) return;
    const first = buttons[0];
    const last = buttons[buttons.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-black/50 p-4">
      <div
        ref={box}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        onKeyDown={onKeyDown}
        className="w-full max-w-[440px] rounded-lg border border-divider bg-surface-raised p-5 shadow-card sm:p-6"
      >
        <h2 id={titleId} className="mb-2 text-lg font-semibold">{title}</h2>
        <div className="mb-5 text-content-muted">{children}</div>
        <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
          <Button variant="secondary" onClick={onCancel}>Cancel</Button>
          <Button onClick={onConfirm}>{confirmLabel}</Button>
        </div>
      </div>
    </div>
  );
}
