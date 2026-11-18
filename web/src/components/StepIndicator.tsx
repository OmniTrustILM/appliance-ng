import { cx } from './cx';

const BAR_WIDTHS = ['w-1/6', 'w-2/6', 'w-3/6', 'w-4/6', 'w-5/6', 'w-full'];

const DOT =
  'before:relative before:z-[1] before:grid before:size-7 before:place-items-center before:rounded-full before:border-2 before:text-[13px] before:font-semibold before:[counter-increment:s]';
const LINE =
  'not-first:after:absolute not-first:after:top-[13px] not-first:after:right-1/2 not-first:after:-left-1/2 not-first:after:h-0.5 not-first:after:content-[""]';

const STATE = {
  done: 'done font-medium text-content before:border-brand before:bg-brand before:text-on-brand dark:before:border-blue-600 dark:before:bg-blue-600 before:content-["✓"]',
  active:
    'font-semibold text-brand before:border-brand before:bg-surface-raised before:text-brand before:shadow-[0_0_0_4px_var(--color-brand-ring)] before:content-[counter(s)]',
  upcoming:
    'font-medium text-content-subtle before:border-border before:bg-surface-raised before:text-content-subtle before:content-[counter(s)]',
};

function stepState(index: number, current: number) {
  if (index < current) return 'done';
  if (index === current) return 'active';
  return 'upcoming';
}

/** Shows the steps of a wizard; current is the index of the step in progress. */
export function StepIndicator({ steps, current }: Readonly<{ steps: string[]; current: number }>) {
  const known = current >= 0;
  const width = BAR_WIDTHS[Math.max(1, Math.round(((current + 1) / steps.length) * 6)) - 1];
  return (
    <>
      {known && (
        <div className="mb-5 sm:hidden">
          <div className="mb-2 flex justify-between text-[13px] text-content-subtle">
            <span>
              Step {current + 1} of {steps.length}
            </span>
            <strong className="font-semibold text-content">{steps[current]}</strong>
          </div>
          <div
            role="progressbar"
            aria-label="Progress"
            aria-valuemin={1}
            aria-valuemax={steps.length}
            aria-valuenow={current + 1}
            className="h-1.5 overflow-hidden rounded-full bg-divider"
          >
            <span className={cx('block h-full rounded-full bg-brand', width)} />
          </div>
        </div>
      )}
      <ol className="mb-6 hidden p-0 [counter-reset:s] sm:flex">
        {steps.map((step, index) => {
          const state = stepState(index, current);
          return (
            <li
              key={step}
              className={cx(
                'relative flex flex-1 flex-col items-center gap-1.5 text-center text-xs',
                DOT,
                LINE,
                STATE[state],
                index <= current ? 'not-first:after:bg-brand' : 'not-first:after:bg-border',
              )}
              aria-current={state === 'active' ? 'step' : undefined}
            >
              {step}
            </li>
          );
        })}
      </ol>
    </>
  );
}
