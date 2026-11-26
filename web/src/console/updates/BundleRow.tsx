import { useState } from 'react';
import { api, timeoutSignal } from '../../api/client';
import type { components } from '../../api/schema';
import { Badge } from '../../components/Badge';
import { Button } from '../../components/Button';
import { attempt, unreachable } from '../../setup/failure';
import { formatBytes } from '../shared/format';
import { ItemRow } from '../shared/ItemRow';
import { blockedText } from './reasons';

type Bundle = components['schemas']['UpdateBundle'];

const noNotes = 'There are no release notes for this bundle.';

/** The release notes of a bundle, read when first opened and shown as plain text. */
function Notes({ bundle }: Readonly<{ bundle: Bundle }>) {
  const [text, setText] = useState<string>();
  const [open, setOpen] = useState(false);

  async function toggle() {
    setOpen(!open);
    if (text !== undefined) return;
    const result = await attempt(() => api.GET('/updates/notes/{kind}/{version}', {
      params: { path: { kind: bundle.kind, version: bundle.version } },
      parseAs: 'text',
      signal: timeoutSignal(),
    }));
    if (!result) setText(unreachable);
    else setText(result.response.ok ? String(result.data) : noNotes);
  }

  return (
    <>
      <Button variant="ghost" aria-expanded={open} aria-label={`Release notes of ${bundle.version}`} onClick={() => void toggle()}>
        Release notes
      </Button>
      {open && <pre className="m-0 mt-2 basis-full whitespace-pre-wrap font-mono text-[13px]">{text ?? 'Loading…'}</pre>}
    </>
  );
}

/** One bundle: its version, kind, size and compatibility, with its release notes and, when it can be loaded, "Fetch". */
export function BundleRow({ bundle, onFetch, fetchDisabled }: Readonly<{ bundle: Bundle; onFetch?: () => void; fetchDisabled?: boolean }>) {
  const details: string[] = [formatBytes(bundle.sizeBytes), bundle.arch];
  if (bundle.ilm) details.push(`ILM ${bundle.ilm}`);
  if (bundle.operator) details.push(`operator ${bundle.operator}`);
  if (bundle.loadedAt) details.push(`loaded ${new Date(bundle.loadedAt).toLocaleString()}`);
  return (
    <ItemRow
      actions={onFetch && !bundle.blocked && (
        <Button variant="secondary" disabled={fetchDisabled} aria-label={`Fetch ${bundle.version}`} onClick={onFetch}>Fetch</Button>
      )}
    >
      <p className="m-0">
        <span className="font-mono text-[13px]">{bundle.version}</span>{' '}
        <Badge tone="muted">{bundle.kind === 'system' ? 'System' : 'Platform'}</Badge>
      </p>
      <p className="m-0 text-content-muted">{details.join(' · ')}</p>
      {bundle.blocked && (
        <p className="m-0 mt-1"><Badge tone="wait">Blocked</Badge> <span className="text-content-muted">{blockedText(bundle.blocked, bundle.needs, bundle.kind)}</span></p>
      )}
      <div className="mt-1 flex flex-wrap items-start"><Notes bundle={bundle} /></div>
    </ItemRow>
  );
}
