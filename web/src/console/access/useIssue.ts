import { useState } from 'react';
import { attempt, failureMessage, unreachable } from '../../setup/failure';
import { downloadBlob } from '../certificate/download';

type Issued = { response: Response; data?: Blob; error?: { message: string; field?: string } };

/** The file name a Content-Disposition header gives, if any. */
function fileNameOf(disposition: string | null) {
  return /filename="([^"]+)"/.exec(disposition ?? '')?.[1];
}

/**
 * Sends a request that answers with an issued file and hands the file to the browser as a download; the file is
 * never kept in state. The server's field errors show at their field, other errors above the form.
 */
export function useIssue(fallbackName: string, onIssued: (filename: string) => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  async function issue(send: () => Promise<Issued>) {
    setBusy(true);
    setError(undefined);
    setFieldErrors({});
    const result = await attempt(send);
    setBusy(false);
    if (!result) return setError(unreachable);
    const { response, data, error: failure } = result;
    if (response.ok && data) {
      const filename = fileNameOf(response.headers.get('Content-Disposition')) ?? fallbackName;
      downloadBlob(filename, data);
      return onIssued(filename);
    }
    const message = failure?.message ?? failureMessage(response.status);
    if (failure?.field) return setFieldErrors({ [failure.field]: message });
    setError(message);
  }

  return { busy, error, fieldErrors, issue: (send: () => Promise<Issued>) => void issue(send) };
}
