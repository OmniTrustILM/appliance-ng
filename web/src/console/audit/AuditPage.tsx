import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { useAuditPages, type AuditEntry } from './useAuditPages';

const results = {
  success: { tone: 'ok', label: 'Success' },
  failure: { tone: 'bad', label: 'Failed' },
  denied: { tone: 'wait', label: 'Denied' },
} as const;

const CELL = 'px-3 py-2.5 align-top';

/**
 * Why a change failed or was denied, and the details it is made of: the server's message, or for an internal error
 * the request ID its log line carries.
 */
function reason(entry: AuditEntry): { text?: string; keys: string[] } {
  if (entry.result === 'success') return { keys: [] };
  const { code, message, requestId } = entry.details ?? {};
  if (code === 'internal' && requestId) return { text: `Internal error, request ${requestId}`, keys: ['message', 'requestId'] };
  return { text: message, keys: ['message'] };
}

/** The changed paths of an entry, or its other details, such as what an issued or revoked credential grants. */
function changes(entry: AuditEntry, shown: readonly string[]) {
  if (entry.paths.length > 0) return entry.paths.join(', ');
  return Object.entries(entry.details ?? {})
    .filter(([key]) => !shown.includes(key))
    .map(([key, value]) => `${key}: ${value}`)
    .join(', ');
}

function EntryRow({ entry }: Readonly<{ entry: AuditEntry }>) {
  const result = results[entry.result];
  const why = reason(entry);
  return (
    <tr className="border-t border-divider">
      <td className={`${CELL} whitespace-nowrap`}>{new Date(entry.time).toLocaleString()}</td>
      <td className={CELL}>{entry.user || '—'}</td>
      <td className={`${CELL} whitespace-nowrap font-mono text-[13px]`}>{entry.action}</td>
      <td className={`${CELL} [overflow-wrap:anywhere]`}>
        <div className="font-mono text-[13px]">{changes(entry, why.keys) || '—'}</div>
        {why.text && <div className="mt-1 text-content-muted">{why.text}</div>}
      </td>
      <td className={CELL}><Badge tone={result.tone}>{result.label}</Badge></td>
    </tr>
  );
}

function EntryTable({ entries }: Readonly<{ entries: readonly AuditEntry[] }>) {
  return (
    <div className="-mx-5 overflow-x-auto sm:mx-0">
      <table className="w-full min-w-[640px] border-collapse text-left text-sm">
        <thead className="text-xs uppercase tracking-[0.06em] text-content-subtle">
          <tr>
            <th scope="col" className={`${CELL} font-semibold`}>Time</th>
            <th scope="col" className={`${CELL} font-semibold`}>User</th>
            <th scope="col" className={`${CELL} font-semibold`}>Action</th>
            <th scope="col" className={`${CELL} font-semibold`}>Changes</th>
            <th scope="col" className={`${CELL} font-semibold`}>Result</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry) => <EntryRow key={`${entry.time}|${entry.user}|${entry.action}|${entry.result}`} entry={entry} />)}
        </tbody>
      </table>
    </div>
  );
}

/** The audit log, newest first, a page at a time; read-only. */
export function AuditPage() {
  const { shown, error, loading, hasNext, next, previous, refresh } = useAuditPages();
  const empty = shown?.entries.length === 0;

  return (
    <PageWidth wide>
      <Card
        back={<Button variant="secondary" disabled={!shown || shown.page === 0 || loading} onClick={previous}>Previous</Button>}
        actions={(
          <>
            <Button variant="secondary" busy={loading && !!shown} onClick={refresh}>Refresh</Button>
            <Button variant="secondary" disabled={!hasNext || loading} onClick={next}>Next</Button>
          </>
        )}
      >
        <PageHeading eyebrow="Platform" title="Audit log" lead="Sign-ins and changes made in this console, newest first." />
        {error && <Alert tone="error">{error}</Alert>}
        {!shown && !error && <Loading />}
        {empty && (
          <p className="text-content-muted">{shown.page === 0 ? 'Nothing has been recorded yet.' : 'No older entries are left.'}</p>
        )}
        {shown && !empty && (
          <>
            <EntryTable entries={shown.entries} />
            <p className="mt-3 text-sm text-content-muted">Page {shown.page + 1}</p>
          </>
        )}
      </Card>
    </PageWidth>
  );
}
