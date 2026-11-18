import type { ButtonHTMLAttributes } from 'react';
import { cx } from './cx';
import { Spinner } from './Spinner';

type ButtonProps = Readonly<
  ButtonHTMLAttributes<HTMLButtonElement> & {
    variant?: Variant;
    busy?: boolean;
    block?: boolean;
  }
>;

const base =
  'inline-flex cursor-pointer items-center justify-center gap-2 whitespace-nowrap border font-medium no-underline disabled:cursor-not-allowed disabled:opacity-50';

const variants = {
  primary: 'h-10 rounded-md border-transparent bg-brand px-[18px] text-white hover:bg-brand-hover dark:bg-blue-600 dark:hover:bg-blue-700',
  secondary:
    'h-10 rounded-md border-border bg-surface-raised px-[18px] text-content hover:bg-surface-sunken',
  ghost:
    'h-10 rounded-md border-transparent bg-transparent px-[18px] text-content-muted hover:bg-surface-hover hover:text-content',
  'on-brand':
    'h-8 rounded-md border-white/55 bg-transparent px-3 text-[13px] text-white hover:bg-white/12 focus-visible:outline-white',
};

type Variant = keyof typeof variants;

/** The classes of a button variant, for a link that should look like one. */
export function buttonClass(variant: Variant, { block = false }: { block?: boolean } = {}) {
  return cx(base, variants[variant], block && 'w-full');
}

/** A button in one of four variants; `busy` disables it and shows a spinner. */
export function Button({
  variant = 'primary',
  busy = false,
  block = false,
  type = 'button',
  disabled,
  className,
  children,
  ...rest
}: ButtonProps) {
  return (
    <button
      {...rest}
      type={type}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      className={cx(buttonClass(variant, { block }), className)}
    >
      {busy && <Spinner onBrand={variant === 'primary' || variant === 'on-brand'} />}
      {children}
    </button>
  );
}
