/** How long the object URL of a download lives; the browser starts reading it when the click is handled. */
const revokeAfterMs = 10000;

/** Offers a file for download. The URL is revoked after the download has started, never synchronously. */
export function downloadBlob(filename: string, blob: Blob) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), revokeAfterMs);
}

/** Offers PEM text as a file download. */
export function downloadText(filename: string, text: string) {
  downloadBlob(filename, new Blob([text], { type: 'application/x-pem-file' }));
}
