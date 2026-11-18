import type { ReactNode } from 'react';
import { cx } from './cx';

type BadgeProps = Readonly<{ tone: 'ok' | 'wait' | 'bad' | 'muted'; children: ReactNode }>;

const tones = {
  ok: 'bg-success-surface text-success',
  wait: 'bg-warning-surface text-warning',
  bad: 'bg-danger-surface text-danger',
  muted: 'bg-surface-sunken text-content-subtle',
};

/** A small status pill with a leading dot. */
export function Badge({ tone, children }: BadgeProps) {
  return (
    <span
      className={cx(
        'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold',
        "before:size-1.5 before:rounded-full before:bg-current before:content-['']",
        tones[tone],
      )}
    >
      {children}
    </span>
  );
}
