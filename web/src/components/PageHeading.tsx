import type { ReactNode } from 'react';
import { cx } from './cx';

/** The page title with an optional eyebrow above it and a lead below it. */
export function PageHeading({ eyebrow, title, lead, centered = false }: Readonly<{ eyebrow?: 'Setup' | 'Platform'; title: string; lead?: ReactNode; centered?: boolean }>) {
  return (
    <>
      {eyebrow && <p className="mb-1 text-xs font-semibold uppercase tracking-[0.06em] text-brand">{eyebrow}</p>}
      <h1 className={cx('mb-1.5 text-[21px] font-semibold leading-tight tracking-[-0.01em] sm:text-2xl', centered && 'text-center')}>{title}</h1>
      {lead && <p className="mb-6 text-content-muted">{lead}</p>}
    </>
  );
}
