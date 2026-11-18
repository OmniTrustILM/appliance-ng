import type { ReactNode } from 'react';
import { cx } from './cx';
import { Spinner } from './Spinner';

type AlertProps = Readonly<{
  tone: 'error' | 'warning' | 'info' | 'success';
  title?: string;
  busy?: boolean;
  children: ReactNode;
}>;

const tones = {
  error: { box: 'border-danger/35 bg-danger-surface text-danger', icon: 'bg-danger' },
  warning: { box: 'border-warning/35 bg-warning-surface text-warning', icon: 'bg-warning' },
  info: { box: 'border-info/30 bg-info-surface text-info', icon: 'bg-info' },
  success: { box: 'border-success/35 bg-success-surface text-success', icon: 'bg-success' },
};

/** A tinted message box; errors are announced as alerts, the rest as status output. */
export function Alert({ tone, title, busy = false, children }: AlertProps) {
  const Box = tone === 'error' ? 'div' : 'output';
  return (
    <Box
      role={tone === 'error' ? 'alert' : undefined}
      className={cx('mb-[18px] flex gap-2.5 rounded-md border px-3.5 py-3 text-sm', tones[tone].box)}
    >
      <span
        aria-hidden="true"
        className={cx(
          'grid size-5 flex-none place-items-center rounded-full text-xs font-bold text-surface-raised',
          !busy && tones[tone].icon,
        )}
      >
        {busy ? <Spinner /> : '!'}
      </span>
      <span className="block text-content">
        {title && <strong className="block">{title}</strong>}
        {children}
      </span>
    </Box>
  );
}
