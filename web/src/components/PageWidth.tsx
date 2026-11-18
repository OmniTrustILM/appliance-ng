import type { ReactNode } from 'react';

/** Centres the page content; 640 px wide, or 1040 px with `wide`. */
export function PageWidth({ children, wide = false }: Readonly<{ children: ReactNode; wide?: boolean }>) {
  return <div className={wide ? 'mx-auto w-full max-w-[1040px]' : 'mx-auto w-full max-w-[640px]'}>{children}</div>;
}
