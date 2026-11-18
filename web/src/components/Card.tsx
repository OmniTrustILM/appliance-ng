import type { ReactNode } from 'react';

/** A raised panel; the footer bar appears only when back or actions is given. */
export function Card({ children, back, actions }: Readonly<{ children: ReactNode; back?: ReactNode; actions?: ReactNode }>) {
  const hasFooter = Boolean(back) || Boolean(actions);
  return (
    <div className="rounded-lg border border-divider bg-surface-raised shadow-card">
      <div className="p-5 sm:p-7">{children}</div>
      {hasFooter && (
        <footer className="flex flex-col-reverse gap-3 rounded-b-lg border-t border-divider bg-surface-sunken px-5 py-3.5 sm:flex-row sm:justify-between sm:px-7 sm:py-4">
          {back && <div className="flex flex-col sm:flex-row [&>*]:w-full sm:[&>*]:w-auto">{back}</div>}
          {actions && (
            <div className="flex flex-col-reverse gap-2 sm:ml-auto sm:flex-row [&>*]:w-full sm:[&>*]:w-auto">{actions}</div>
          )}
        </footer>
      )}
    </div>
  );
}
