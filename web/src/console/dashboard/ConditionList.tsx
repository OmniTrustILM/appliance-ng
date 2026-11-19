import { Badge } from '../../components/Badge';
import { Spinner } from '../../components/Spinner';
import type { Status } from './useStatus';

const MARKER = 'grid size-[22px] flex-none place-items-center rounded-full';

/** True is a tick, False an empty dot, Unknown (or no status) a spinner; the text carries the meaning. */
function Marker({ status }: Readonly<{ status?: string }>) {
  if (status === 'True') {
    return <span aria-hidden="true" className={`${MARKER} bg-success text-xs font-bold text-surface-raised`}>✓</span>;
  }
  if (status === 'False') return <span aria-hidden="true" className={`${MARKER} border-2 border-border`} />;
  return <span className={MARKER}><Spinner /></span>;
}

/** Shows conditions with their reasons and messages: marked steps for the progress page, badges for the status page. */
export function ConditionList({ conditions, markers = false }: Readonly<{ conditions: Status['conditions']; markers?: boolean }>) {
  return (
    <ul className={markers ? 'm-0 list-none p-0' : 'm-0 list-none divide-y divide-divider p-0'}>
      {conditions.map(({ type, status, reason, message }) => {
        if (markers) {
          return (
            <li key={type} className="flex items-start gap-3 py-2.5 text-sm">
              <Marker status={status} />
              <div className="min-w-0 [overflow-wrap:anywhere]">
                <strong>{type}</strong>: {status}
                {reason && <> (<span>{reason}</span>)</>}
                {message && <p className="mt-0.5 min-w-0 text-content-muted [overflow-wrap:anywhere]">{message}</p>}
              </div>
            </li>
          );
        }
        return (
          <li key={type} className="grid grid-cols-[1fr_auto] gap-x-3 gap-y-0.5 py-3 text-sm">
            <div className="min-w-0 [overflow-wrap:anywhere]">
              <strong className="font-medium">{type}</strong>
              {reason && <span className="ml-2 font-mono text-xs text-content-subtle">{reason}</span>}
            </div>
            <Badge tone={status === 'True' ? 'ok' : 'wait'}>{status}</Badge>
            {message && <p className="col-span-2 min-w-0 text-[13px] text-content-muted [overflow-wrap:anywhere]">{message}</p>}
          </li>
        );
      })}
    </ul>
  );
}
