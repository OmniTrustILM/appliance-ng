import { Link } from 'react-router';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Card } from '../../components/Card';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { ConditionList } from './ConditionList';
import { useStatus } from './useStatus';

/** The console's start page: the state of the appliance, refreshed every five seconds. */
export function StatusPage() {
  const { status, error } = useStatus();
  const complete = status?.setup === 'complete';
  const ready = status?.conditions.filter((c) => c.status === 'True').length ?? 0;

  return (
    <PageWidth wide>
      <div className="flex items-start justify-between gap-3">
        <div>
          <PageHeading
            title="Appliance"
            lead={
              <>
                {status?.nodeAddress && <><span className="font-mono">{status.nodeAddress}</span> · </>}
                Refreshed every five seconds
              </>
            }
          />
        </div>
        {status && <Badge tone={complete ? 'ok' : 'wait'}>{complete ? 'Setup complete' : 'Setup in progress'}</Badge>}
      </div>
      {error && <Alert tone="error">{error}</Alert>}
      {!status && !error && <Loading />}
      {status && (
        <>
          {status.rebootRequired && <Alert tone="warning">The node needs to restart to apply a change.</Alert>}
          <div className="grid gap-4 sm:grid-cols-2">
            <Card>
              <h2 className="mb-3 text-base font-semibold">Setup</h2>
              <p>{complete ? 'Setup is complete.' : 'Setup is not complete.'}</p>
              <p className="mt-1 text-sm">
                {complete
                  ? <Link className="text-brand hover:underline" to="/wizard/done">ILM address and administrator credential</Link>
                  : <Link className="text-brand hover:underline" to="/wizard/">Continue the setup</Link>}
              </p>
              {status.addonStep && <p className="mt-3 text-sm text-content-muted">Installing add-on: {status.addonStep}</p>}
            </Card>
            <Card>
              <h2 className="mb-3 text-base font-semibold">Versions</h2>
              <dl className="m-0">
                {Object.entries(status.versions).map(([name, version]) => (
                  <div key={name} className="flex justify-between gap-3 border-b border-divider py-2 text-sm last:border-b-0">
                    <dt className="text-content-muted">{name}</dt>
                    <dd className="m-0 font-mono text-[13px]">{version}</dd>
                  </div>
                ))}
              </dl>
            </Card>
            <div className="sm:col-span-2">
              <Card>
                <div className="mb-3 flex items-baseline justify-between">
                  <h2 className="text-base font-semibold">Conditions</h2>
                  <small className="text-xs text-content-muted">{ready} of {status.conditions.length} ready</small>
                </div>
                <ConditionList conditions={status.conditions} />
              </Card>
            </div>
          </div>
        </>
      )}
    </PageWidth>
  );
}
