import type { ReactNode } from 'react';

/** One term and its value in a summary list; `mono` sets the value in the monospace face. */
export function SummaryRow({ term, mono, children }: Readonly<{ term: string; mono?: boolean; children: ReactNode }>) {
  return (
    <div className="grid gap-0.5 border-b border-divider py-2.5 text-sm sm:grid-cols-[180px_1fr] sm:gap-3">
      <dt className="min-w-0 text-content-subtle">{term}</dt>
      <dd className={mono ? 'm-0 min-w-0 font-mono font-medium [overflow-wrap:anywhere]' : 'm-0 min-w-0 font-medium [overflow-wrap:anywhere]'}>{children}</dd>
    </div>
  );
}
