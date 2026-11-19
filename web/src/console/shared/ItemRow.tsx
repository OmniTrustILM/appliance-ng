import type { ReactNode } from 'react';

/** One entry of a list: its details on the left and its actions beside them, below them on a narrow screen. */
export function ItemRow({ actions, children }: Readonly<{ actions: ReactNode; children: ReactNode }>) {
  return (
    <li className="flex flex-col gap-3 border-b border-divider py-3 last:border-b-0 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0 flex-1 text-sm [overflow-wrap:anywhere]">{children}</div>
      <div className="flex flex-none gap-2">{actions}</div>
    </li>
  );
}
