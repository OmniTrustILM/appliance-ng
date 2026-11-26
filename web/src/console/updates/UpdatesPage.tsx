import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { api, timeoutSignal } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, unreachable } from '../../setup/failure';
import { useLoaded } from '../shared/useLoaded';
import { BundleRow } from './BundleRow';
import { InstallConfirm, UpdateStatus, updating } from './PlatformUpdate';
import { checkErrorText, loadReasonText, refusalText, rollbackUnavailableText, updateRefusalText } from './reasons';
import { RollbackCard, RollbackUnavailable } from './Rollback';
import { UploadCard } from './UploadCard';

type Load = components['schemas']['BundleLoad'];
type Bundle = components['schemas']['UpdateBundle'];

/** How often the page reads the updates again while a bundle loads or an update runs. */
export const runningRefreshMs = 5000;
/** How long the page waits for a check: longer than the appliance takes at most to check, record and answer. */
export const checkTimeoutMs = 6 * 60_000;
/** How many reads after a load was accepted look for it before the page stops looking. */
export const startedReads = 6;

function LoadStatus({ load }: Readonly<{ load: Load }>) {
  const bundle = load.version ? `${load.kind} ${load.version}` : `the ${load.kind} bundle`;
  if (load.state === 'running') return <Alert tone="info" busy>Loading {bundle}…</Alert>;
  if (load.state === 'loaded') return <Alert tone="success">{bundle.charAt(0).toUpperCase() + bundle.slice(1)} was loaded.</Alert>;
  return <Alert tone="error">The bundle could not be loaded. {loadReasonText(load.reason, load.kind)}</Alert>;
}

function Section({ title, empty, bundles, children }: Readonly<{ title: string; empty: string; bundles: Bundle[]; children: (bundle: Bundle) => ReactNode }>) {
  return (
    <section aria-label={title} className="mt-6">
      <Card>
        <h2 className="mb-1.5 text-lg font-semibold">{title}</h2>
        {bundles.length === 0 && <p className="text-content-muted">{empty}</p>}
        <ul className="m-0 list-none p-0">{bundles.map(children)}</ul>
      </Card>
    </section>
  );
}

/** Whether bundle is among the loaded ones, which need no fetch. */
function isLoaded(loaded: Bundle[], bundle: Bundle) {
  return loaded.some((l) => l.kind === bundle.kind && l.version === bundle.version && l.arch === bundle.arch);
}

/** The system and platform bundles available online and loaded, with "Check now", "Fetch", "Install" and an upload. */
export function UpdatesPage() {
  const background = useRef(false);
  // Marked as a background refresh, a read the page makes on its own does not keep the session from going idle.
  const loadUpdates = useCallback(
    () => api.GET('/updates', {
      ...(background.current ? { params: { header: { 'X-Background-Refresh': '1' } } } : {}),
      signal: timeoutSignal(),
    }),
    [],
  );
  const { data, error, reload: load, settled } = useLoaded(loadUpdates);
  const [checking, setChecking] = useState(false);
  const [failure, setFailure] = useState<string>();
  // Reads left to find a load that was accepted but does not show as running yet.
  const [awaited, setAwaited] = useState(0);
  const [installing, setInstalling] = useState<Bundle>();
  const running = data?.load?.state === 'running' || updating(data?.update);

  const reload = useCallback(() => {
    background.current = false;
    load();
  }, [load]);

  useEffect(() => {
    if (!running && awaited === 0) return undefined;
    const timer = setTimeout(() => {
      setAwaited((left) => (running ? 0 : left - 1));
      background.current = true;
      load();
    }, runningRefreshMs);
    return () => clearTimeout(timer);
  }, [running, awaited, settled, load]);

  function started() {
    setAwaited(startedReads);
    reload();
  }

  async function checkNow() {
    setChecking(true);
    setFailure(undefined);
    const result = await attempt(() => api.POST('/updates/check', { signal: timeoutSignal(checkTimeoutMs) }));
    setChecking(false);
    if (!result) setFailure(unreachable);
    else if (!result.response.ok) setFailure('The online repository could not be checked.');
    reload();
  }

  async function fetchBundle(bundle: Bundle) {
    setFailure(undefined);
    const body = { kind: bundle.kind, version: bundle.version };
    const result = await attempt(() => api.POST('/updates/fetch', { body, signal: timeoutSignal() }));
    if (!result) return setFailure(unreachable);
    if (!result.response.ok) return setFailure(refusalText(result.error?.code, 'The bundle could not be fetched.'));
    started();
  }

  async function install(bundle: Bundle) {
    setInstalling(undefined);
    setFailure(undefined);
    const body = { version: bundle.version, confirm: true };
    const result = await attempt(() => api.POST('/updates/platform', { body, signal: timeoutSignal() }));
    if (!result) return setFailure(unreachable);
    if (!result.response.ok) return setFailure(updateRefusalText(result.error?.code));
    started();
  }

  const busy = checking || running;
  const unavailable = data?.update && !running ? rollbackUnavailableText(data.rollback?.reason) : undefined;

  return (
    <PageWidth>
      <Card>
        <PageHeading title="Updates" lead="System and platform bundles, loaded from the online repository or uploaded. Loading a bundle does not install it." />
        {error && <Alert tone="error">{error}</Alert>}
        {failure && <Alert tone="error">{failure}</Alert>}
        {!data && !error && <Loading />}
        {data && (
          <>
            {data.update && <UpdateStatus update={data.update} />}
            {data.load && <LoadStatus load={data.load} />}
            {data.lastCheckError && <Alert tone="warning">The last check failed. {checkErrorText(data.lastCheckError)}</Alert>}
            {checking && <Alert tone="info" busy>Checking the online repository…</Alert>}
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p className="m-0 text-content-muted">
                {data.lastCheck ? `Last checked ${new Date(data.lastCheck).toLocaleString()}.` : 'The online repository was not checked yet.'}
              </p>
              <Button busy={checking} disabled={busy} onClick={() => void checkNow()}>Check now</Button>
            </div>
          </>
        )}
      </Card>

      {data && (
        <>
          {data.rollback?.available && !running && (
            <div className="mt-6"><RollbackCard backup={data.update?.backup} disabled={busy} onStarted={started} /></div>
          )}
          {!data.rollback?.available && unavailable && <RollbackUnavailable text={unavailable} />}
          <Section title="Available" empty="No newer bundle is available." bundles={data.available}>
            {(b) => (
              <BundleRow
                key={`${b.kind}-${b.version}`}
                bundle={b}
                disabled={busy}
                onFetch={isLoaded(data.loaded, b) ? undefined : () => void fetchBundle(b)}
              />
            )}
          </Section>
          <Section title="Loaded" empty="No bundle is loaded." bundles={data.loaded}>
            {(b) => (
              <BundleRow
                key={`${b.kind}-${b.version}`}
                bundle={b}
                disabled={busy}
                onInstall={b.kind === 'platform' ? () => setInstalling(b) : undefined}
              />
            )}
          </Section>
          <div className="mt-6"><UploadCard disabled={busy} onUploaded={started} /></div>
          {installing && <InstallConfirm bundle={installing} operator={data.operator} onConfirm={() => void install(installing)} onCancel={() => setInstalling(undefined)} />}
        </>
      )}
    </PageWidth>
  );
}
