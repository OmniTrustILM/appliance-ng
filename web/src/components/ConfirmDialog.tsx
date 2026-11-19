import { useEffect, useId, useRef, type ReactNode, type SyntheticEvent } from 'react';
import { Button } from './Button';

type ConfirmDialogProps = Readonly<{
  title: string;
  confirmLabel: string;
  children: ReactNode;
  onConfirm: () => void;
  onCancel: () => void;
}>;

/** A modal confirmation in a native dialog: focus starts on Cancel, the browser keeps Tab inside and Escape cancels, and focus returns to the opener. */
export function ConfirmDialog({ title, confirmLabel, children, onConfirm, onCancel }: ConfirmDialogProps) {
  const titleId = useId();
  const box = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = box.current;
    const opener = document.activeElement;
    dialog?.showModal();
    dialog?.querySelector('button')?.focus();
    return () => {
      dialog?.close();
      if (opener instanceof HTMLElement) opener.focus();
    };
  }, []);

  // The browser fires cancel on Escape; the parent decides whether the dialog goes away.
  function onCancelEvent(event: SyntheticEvent) {
    event.preventDefault();
    onCancel();
  }

  return (
    <dialog
      ref={box}
      aria-labelledby={titleId}
      onCancel={onCancelEvent}
      className="m-auto w-full max-w-[440px] rounded-lg border border-divider bg-surface-raised p-5 text-content shadow-card backdrop:bg-black/50 sm:p-6"
    >
      <h2 id={titleId} className="mb-2 text-lg font-semibold">{title}</h2>
      <div className="mb-5 text-content-muted">{children}</div>
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onCancel}>Cancel</Button>
        <Button onClick={onConfirm}>{confirmLabel}</Button>
      </div>
    </dialog>
  );
}
