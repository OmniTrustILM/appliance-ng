import type { Status } from './useStatus';

/** Shows conditions with their reasons and messages; a condition that is not True stands out. */
export function ConditionList({ conditions }: { conditions: Status['conditions'] }) {
  return (
    <ul className="conditions">
      {conditions.map(({ type, status, reason, message }) => (
        <li key={type} className={status === 'True' ? 'condition-ok' : 'condition-pending'}>
          <strong>{type}</strong>: {status}
          {reason && <> (<span>{reason}</span>)</>}
          {message && <p>{message}</p>}
        </li>
      ))}
    </ul>
  );
}
