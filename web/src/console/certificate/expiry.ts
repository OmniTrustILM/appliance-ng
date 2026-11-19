const warnWithinMs = 30 * 24 * 60 * 60 * 1000;

/** The warning for a certificate that expired or expires within 30 days of now, or undefined for none. */
export function expiryNotice(notAfter: Date, now: number) {
  if (notAfter.getTime() <= now) return 'The certificate has expired. Replace it now.';
  if (notAfter.getTime() - now <= warnWithinMs) return 'The certificate expires within 30 days. Replace it before then.';
  return undefined;
}
