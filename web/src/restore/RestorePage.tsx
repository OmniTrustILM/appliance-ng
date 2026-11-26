import { useCallback, useEffect, useRef, useState } from 'react';
import { Link } from 'react-router';
import { api, timeoutSignal } from '../api/client';
import type { components } from '../api/schema';
import { Alert } from '../components/Alert';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';
import { PageWidth } from '../components/PageWidth';
import { Loading } from '../components/Spinner';
import { useLoaded } from '../console/shared/useLoaded';
import { FreshRestore } from './FreshRestore';
import { ProgressView } from './ProgressView';
import { isRunning } from './steps';

type State = components['schemas']['RestoreState'];

/** How often the page reads the progress again while a restore runs. */
export const progressRefreshMs = 5000;

type ContentProps = Readonly<{ state: State; startOver: boolean; onStartOver: () => void; onChanged: () => void }>;

/** The progress of the latest restore, the restore of a fresh appliance, or a note that none waits. */
function Content({ state, startOver, onStartOver, onChanged }: ContentProps) {
  const fresh = state.setup === 'restore';
  const progress = state.restore;
  if (progress && !(fresh && startOver) && (!['done', 'abandoned'].includes(progress.step) || !fresh)) {
    return <ProgressView progress={progress} fresh={fresh} onResumed={onChanged} onStartOver={onStartOver} />;
  }
  if (fresh) return <FreshRestore onStarted={onChanged} />;
  return (
    <p className="text-content-muted">
      No restore is in progress. A backup on this appliance is restored in place from the <Link to="/backups">Backups</Link> page.
    </p>
  );
}

/** Restores a backup onto a fresh appliance and follows any restore to its end. */
export function RestorePage() {
  const background = useRef(false);
  // Marked as a background refresh, a read the page makes on its own does not keep the session from going idle.
  const loadState = useCallback(
    () => api.GET('/restore', {
      ...(background.current ? { params: { header: { 'X-Background-Refresh': '1' } } } : {}),
      signal: timeoutSignal(),
    }),
    [],
  );
  const { data, error, reload: load, settled } = useLoaded(loadState);
  const [startOver, setStartOver] = useState(false);
  const running = isRunning(data?.restore);

  useEffect(() => {
    if (!running) return undefined;
    const timer = setTimeout(() => {
      background.current = true;
      load();
    }, progressRefreshMs);
    return () => clearTimeout(timer);
  }, [running, settled, load]);

  const changed = useCallback(() => {
    background.current = false;
    setStartOver(false);
    load();
  }, [load]);

  return (
    <PageWidth>
      <Card>
        <PageHeading title="Restore a backup"
          lead="A backup brings back the appliance's settings, its secrets and the ILM database, encrypted to the backup key of its recovery kit." />
        {error && <Alert tone="error">{error}</Alert>}
        {!data && !error && <Loading />}
        {data && <Content state={data} startOver={startOver} onStartOver={() => setStartOver(true)} onChanged={changed} />}
      </Card>
    </PageWidth>
  );
}
