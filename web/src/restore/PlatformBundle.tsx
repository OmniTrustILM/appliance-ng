import { useEffect, useState } from 'react';
import { api, timeoutSignal } from '../api/client';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { FileDropField } from '../components/FileDropField';
import { loadReasonText, refusalText } from '../console/updates/reasons';
import { uploadBundle } from '../console/updates/uploadBundle';
import { unreachable } from '../setup/failure';

/** How often the load of the uploaded bundle is read again while it runs. */
export const bundleRefreshMs = 5000;

type State = { phase: 'idle' } | { phase: 'uploading'; percent: number } | { phase: 'loading' } | { phase: 'loaded' } | { phase: 'failed'; message: string };

/** Reads the latest bundle load: its state and why it was refused or failed; undefined when it cannot be read. */
async function latestLoad() {
  const result = await api.GET('/updates', { params: { header: { 'X-Background-Refresh': '1' } }, signal: timeoutSignal() }).catch(() => undefined);
  return result?.data?.load;
}

/**
 * Uploads the platform bundle of the ILM version a backup holds, for an appliance that runs another one, and follows its
 * load; the backup is then checked again with the kit.
 */
export function PlatformBundle() {
  const [file, setFile] = useState<File>();
  const [state, setState] = useState<State>({ phase: 'idle' });

  useEffect(() => {
    if (state.phase !== 'loading') return undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;
    async function follow() {
      const load = await latestLoad();
      if (stopped) return;
      if (!load || load.state === 'running') {
        timer = setTimeout(() => void follow(), bundleRefreshMs);
        return;
      }
      setState(load.state === 'loaded'
        ? { phase: 'loaded' }
        : { phase: 'failed', message: `The bundle could not be loaded. ${loadReasonText(load.reason, 'platform')}` });
    }
    void follow();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [state.phase]);

  async function upload() {
    if (!file) return;
    setState({ phase: 'uploading', percent: 0 });
    const result = await uploadBundle('platform', file, (loaded, total) => setState({ phase: 'uploading', percent: total > 0 ? Math.round((loaded / total) * 100) : 0 }));
    if (result.accepted) return setState({ phase: 'loading' });
    setState({ phase: 'failed', message: result.unreachable ? unreachable : refusalText(result.code, 'The bundle could not be uploaded.') });
  }

  const busy = state.phase === 'uploading' || state.phase === 'loading';
  return (
    <section aria-label="Load the platform bundle of the backup" className="mt-6 border-t border-divider pt-4">
      <h3 className="mb-1.5 font-semibold">Load the platform bundle of the backup</h3>
      <p className="mb-4 text-content-muted">
        A backup is restored with the ILM version it holds. Upload the platform bundle of that ILM version; once it is loaded, check the backup again.
      </p>
      {state.phase === 'failed' && <Alert tone="error">{state.message}</Alert>}
      {state.phase === 'loading' && <Alert tone="info" busy>Checking and loading the bundle…</Alert>}
      {state.phase === 'loaded' && <Alert tone="success">The platform bundle was loaded. Check the backup again.</Alert>}
      {state.phase === 'uploading' && <p className="mb-2 text-[13px] text-content-muted">Uploading: {state.percent}%</p>}
      <FileDropField label="Platform bundle file" required disabled={busy} file={file} onChange={(chosen) => chosen && setFile(chosen)} />
      <Button busy={busy} disabled={!file || busy} onClick={() => void upload()}>Load the bundle</Button>
    </section>
  );
}
