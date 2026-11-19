const shortLength = 16;

/** A SHA-256 fingerprint shortened for reading; screen readers and the tooltip get the full value. */
export function Fingerprint({ sha256 }: Readonly<{ sha256: string }>) {
  return (
    <span className="font-mono">
      <span aria-hidden="true" title={sha256}>{sha256.slice(0, shortLength)}…</span>
      <span className="sr-only">{sha256}</span>
    </span>
  );
}
