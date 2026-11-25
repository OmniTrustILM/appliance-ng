import { useState } from 'react';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { FileDropField } from '../../components/FileDropField';
import { INPUT } from '../../components/Field';
import { unreachable } from '../../setup/failure';
import { refusalText } from './reasons';
import { uploadBundle } from './uploadBundle';

type Kind = components['schemas']['BundleKind'];

/** Uploads a bundle file with its progress; the appliance checks and loads it in the background once it is there. */
export function UploadCard({ disabled, onUploaded }: Readonly<{ disabled: boolean; onUploaded: () => void }>) {
  const [kind, setKind] = useState<Kind>('system');
  const [file, setFile] = useState<File>();
  const [sent, setSent] = useState<{ loaded: number; total: number }>();
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState<string>();
  const [done, setDone] = useState(false);

  async function upload() {
    if (!file) return;
    setUploading(true);
    setDone(false);
    setError(undefined);
    setSent({ loaded: 0, total: file.size });
    const result = await uploadBundle(kind, file, (loaded, total) => setSent({ loaded, total }));
    setUploading(false);
    setSent(undefined);
    if (!result.accepted) {
      setError(result.unreachable ? unreachable : refusalText(result.code, 'The bundle could not be uploaded.'));
      return;
    }
    setFile(undefined);
    setDone(true);
    onUploaded();
  }

  const percent = sent && sent.total > 0 ? Math.round((sent.loaded / sent.total) * 100) : 0;

  return (
    <Card>
      <h2 className="mb-1.5 text-lg font-semibold">Upload a bundle</h2>
      <p className="mb-5 text-content-muted">
        For an appliance that cannot reach the online repository. The bundle is checked and loaded once it has arrived.
      </p>
      {error && <Alert tone="error">{error}</Alert>}
      {done && <Alert tone="success">The bundle was uploaded; it is being checked and loaded.</Alert>}
      <div className="mb-[18px]">
        <label htmlFor="bundle-kind" className="mb-1.5 block text-sm font-medium">Kind</label>
        <select id="bundle-kind" className={INPUT} value={kind} disabled={uploading} onChange={(e) => setKind(e.target.value as Kind)}>
          <option value="system">System</option>
          <option value="platform">Platform</option>
        </select>
      </div>
      <FileDropField label="Bundle file" required file={file} onChange={(chosen) => chosen && setFile(chosen)} />
      {sent && (
        <div className="mb-[18px]">
          <progress aria-label="Upload progress" className="block h-1.5 w-full" value={sent.loaded} max={sent.total} />
          <p className="mt-1 text-[13px] text-content-muted">{percent}%</p>
        </div>
      )}
      <Button busy={uploading} disabled={!file || disabled} onClick={() => void upload()}>Upload</Button>
    </Card>
  );
}
