import { cx } from './cx';

type SpinnerProps = Readonly<{ size?: 'sm' | 'lg'; onBrand?: boolean }>;

/** A decorative rotating ring; the surrounding element carries the accessible name. */
export function Spinner({ size = 'sm', onBrand = false }: SpinnerProps) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        'inline-block shrink-0 animate-spin rounded-full',
        size === 'lg' ? 'size-8 border-[3px]' : 'size-4 border-2',
        onBrand ? 'border-white/40 border-t-white' : 'border-divider border-t-brand',
      )}
    />
  );
}

/** A centred loading indicator announced to assistive technology. */
export function Loading() {
  return (
    <div role="status" className="flex items-center justify-center gap-3 py-8 text-content-muted">
      <Spinner size="lg" />
      <span>Loading…</span>
    </div>
  );
}
