import { sessionExpired, signRequest } from '../../api/client';
import { apiBase } from '../../base';
import type { components } from '../../api/schema';

/** How an upload ended: accepted, or refused with the code of the answer, if it had one. */
export type Upload = { accepted: true } | { accepted: false; code?: string; unreachable?: boolean };

function codeOf(text: string) {
  try {
    return (JSON.parse(text) as { code?: string }).code;
  } catch {
    return undefined;
  }
}

/**
 * Sends a bundle file to the appliance and reports the bytes sent so far. fetch cannot report the progress of an
 * upload, so the request is an XMLHttpRequest.
 */
export function uploadBundle(kind: components['schemas']['BundleKind'], file: File, onProgress: (sent: number, total: number) => void) {
  return new Promise<Upload>((resolve) => {
    const request = new XMLHttpRequest();
    request.open('PUT', `${apiBase}/updates/upload?kind=${kind}`);
    request.setRequestHeader('Content-Type', 'application/octet-stream');
    signRequest(request);
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(event.loaded, event.total);
    };
    request.onload = () => {
      if (request.status === 401) sessionExpired();
      resolve(request.status === 202 ? { accepted: true } : { accepted: false, code: codeOf(request.responseText) });
    };
    request.onerror = () => resolve({ accepted: false, unreachable: true });
    request.onabort = request.onerror;
    request.send(file);
  });
}
