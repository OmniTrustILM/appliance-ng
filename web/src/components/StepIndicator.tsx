/** Shows the steps of a wizard; current is the index of the step in progress. */
export function StepIndicator({ steps, current }: Readonly<{ steps: string[]; current: number }>) {
  return (
    <ol className="steps">
      {steps.map((step, index) => (
        <li
          key={step}
          className={index < current ? 'done' : undefined}
          aria-current={index === current ? 'step' : undefined}
        >
          {step}
        </li>
      ))}
    </ol>
  );
}
