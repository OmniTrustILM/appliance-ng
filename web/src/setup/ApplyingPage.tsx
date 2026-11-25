import { useEffect, useState } from 'react';
import { api, timeoutSignal } from '../api/client';
import { basePath } from '../base';
import { buttonClass } from '../components/Button';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';
import { Spinner } from '../components/Spinner';
import { attempt } from './failure';

const pollMs = 5000;

type Stage = 'Applying the settings' | 'Installing and restarting' | 'Starting the appliance';

/** Where the appliance stands: the setup service still answers, nothing answers yet, or the console starts. */
async function readStage(): Promise<Stage | 'ready'> {
  const state = await attempt(() => api.GET('/setup/state', { signal: timeoutSignal() }));
  if (state?.response.ok) return 'Applying the settings';
  const readiness = await attempt(() => api.GET('/readiness', { signal: timeoutSignal() }));
  if (!readiness?.data) return 'Installing and restarting';
  return readiness.data.signIn ? 'ready' : 'Starting the appliance';
}

function elapsedText(seconds: number) {
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

/**
 * The node installs and reboots. On the address the browser used, the page follows the appliance until the console
 * accepts a sign-in and then opens it. On another address the browser cannot ask the appliance across origins, so
 * the page links to the new address and shows how long the install has run.
 */
export function ApplyingPage({ address }: Readonly<{ address?: string }>) {
  const sameHost = !address || address === window.location.hostname;
  const [stage, setStage] = useState<Stage>('Applying the settings');
  const [start] = useState(() => Date.now());
  const [now, setNow] = useState(start);
  const seconds = Math.floor((now - start) / 1000);

  useEffect(() => {
    const clock = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(clock);
  }, []);

  useEffect(() => {
    if (!sameHost) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    // The next poll waits until this one settled, so a slow answer never lands after a newer one.
    const poll = async () => {
      const next = await readStage();
      if (!active) return;
      if (next === 'ready') window.location.assign(`${basePath}/`);
      else setStage(next);
      timer = setTimeout(() => void poll(), pollMs);
    };
    timer = setTimeout(() => void poll(), pollMs);
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [sameHost]);

  const url = `https://${encodeURIComponent(address ?? '')}:8443/`;
  return (
    <Card>
      <PageHeading eyebrow="Setup" title="Installing" />
      {sameHost ? (
        <output className="mb-2 flex items-center gap-2">
          <Spinner />
          {stage}
        </output>
      ) : (
        <>
          <p className="mb-2">The node installs and restarts now. When it is ready, open the console at</p>
          <p className="mb-4">
            <span className="break-all font-mono">{address}</span>
          </p>
          <a href={url} className={buttonClass('primary')}>Open the console</a>
        </>
      )}
      <p className="mt-4 text-content-muted">Elapsed time <span className="font-mono">{elapsedText(seconds)}</span></p>
    </Card>
  );
}
