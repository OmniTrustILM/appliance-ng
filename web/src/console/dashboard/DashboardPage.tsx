import { useState } from 'react';
import { Link } from 'react-router';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { Card } from '../../components/Card';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { expiryNotice, warnWithinMs } from '../certificate/expiry';
import { ConditionAreas } from './ConditionAreas';
import { useStatus, type Status } from './useStatus';

const LINK = 'text-brand hover:underline';

const versionLabels: ReadonlyArray<readonly [keyof Status['versions'], string]> = [
  ['appliance', 'Appliance'],
  ['talos', 'Talos'],
  ['kubernetes', 'Kubernetes'],
  ['operator', 'ILM operator'],
  ['ilm', 'ILM'],
];

/** The share of free space below which the data volume counts as running low. */
const lowFreeShare = 0.2;

const byteUnits = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

/** Formats a size in binary multiples with one decimal, such as 60.0 GB. */
function formatBytes(bytes: number) {
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < byteUnits.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${byteUnits[unit]}`;
}

/** The warning about the recovery kit: not confirmed once setup is complete, or its talosconfig near its expiry. */
function recoveryKitNotice(status: Status, now: number) {
  const kit = status.recoveryKit;
  if (!kit || status.setup !== 'complete') return undefined;
  if (!kit.confirmed) return 'The recovery kit is not confirmed. Issue and store it before you rely on backups.';
  const notAfter = kit.talosconfigNotAfter ? new Date(kit.talosconfigNotAfter).getTime() : Infinity;
  if (notAfter <= now) return 'The talosconfig in the recovery kit has expired. Issue the kit again now.';
  if (notAfter - now <= warnWithinMs) {
    return 'The talosconfig in the recovery kit expires within 30 days. Issue the kit again before then.';
  }
  return undefined;
}

/**
 * The warnings that ask the administrator to act: a certificate near its expiry, the recovery kit, a data volume
 * running low and a pending restart.
 */
function Warnings({ status }: Readonly<{ status: Status }>) {
  const [now] = useState(Date.now);
  const volume = status.dataVolume;
  const lowSpace = volume && volume.sizeBytes > 0 && volume.freeBytes < volume.sizeBytes * lowFreeShare;
  const certificate = status.certificate && expiryNotice(new Date(status.certificate.notAfter), now);
  const recoveryKit = recoveryKitNotice(status, now);
  return (
    <>
      {recoveryKit && (
        <Alert tone="warning">
          {recoveryKit} <Link className={LINK} to="/recovery-kit">Open the recovery kit page</Link>
        </Alert>
      )}
      {certificate && (
        <Alert tone="warning">
          {certificate} <Link className={LINK} to="/certificate">Open the certificate page</Link>
        </Alert>
      )}
      {lowSpace && (
        <Alert tone="warning">The data volume's free space is running low.</Alert>
      )}
      {status.rebootRequired && (
        <Alert tone="warning">
          The node needs to restart to apply a change. <Link className={LINK} to="/system">Open the system page</Link>
        </Alert>
      )}
    </>
  );
}

/** The console's start page: the state of the appliance, refreshed every five seconds. */
export function DashboardPage() {
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
          <Warnings status={status} />
          <div className="grid gap-4 sm:grid-cols-2">
            <Card>
              <h2 className="mb-3 text-base font-semibold">Setup</h2>
              <p>{complete ? 'Setup is complete.' : 'Setup is not complete.'}</p>
              <p className="mt-1 text-sm">
                {complete
                  ? <Link className={LINK} to="/wizard/done">ILM address and administrator credential</Link>
                  : <Link className={LINK} to="/wizard/">Continue the setup</Link>}
              </p>
              {status.addonStep && <p className="mt-3 text-sm text-content-muted">Installing add-on: {status.addonStep}</p>}
              {status.dataVolume && (
                <p className="mt-3 text-sm text-content-muted">
                  Data volume: {formatBytes(status.dataVolume.freeBytes)} free of {formatBytes(status.dataVolume.sizeBytes)}
                </p>
              )}
              {status.certificate && (
                <p className="mt-3 text-sm text-content-muted">
                  Certificate valid until <span>{new Date(status.certificate.notAfter).toLocaleDateString()}</span>
                </p>
              )}
            </Card>
            <Card>
              <h2 className="mb-3 text-base font-semibold">Versions</h2>
              <dl className="m-0">
                {versionLabels.filter(([key]) => status.versions[key]).map(([key, label]) => (
                  <div key={key} className="flex justify-between gap-3 border-b border-divider py-2 text-sm last:border-b-0">
                    <dt className="text-content-muted">{label}</dt>
                    <dd className="m-0 font-mono text-[13px]">{status.versions[key]}</dd>
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
                <ConditionAreas conditions={status.conditions} />
              </Card>
            </div>
          </div>
        </>
      )}
    </PageWidth>
  );
}
