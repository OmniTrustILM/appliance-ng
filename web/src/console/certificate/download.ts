/** How long the object URL of a download lives; the browser starts reading it when the click is handled. */
const revokeAfterMs = 10000;

/** Offers text as a file download. The URL is revoked after the download has started, never synchronously. */
export function downloadText(filename: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/x-pem-file' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), revokeAfterMs);
}
