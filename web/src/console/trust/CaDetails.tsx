import type { components } from '../../api/schema';
import { Fingerprint } from './Fingerprint';

export type TrustedCertificate = components['schemas']['TrustedCertificate'];

/** A CA certificate as text: subject, issuer, expiry and fingerprint. */
export function CaDetails({ ca }: Readonly<{ ca: TrustedCertificate }>) {
  return (
    <>
      <p className="m-0 font-medium">{ca.subject}</p>
      <p className="m-0 text-content-muted">Issuer: {ca.issuer}</p>
      <p className="m-0 text-content-muted">Expires: {new Date(ca.notAfter).toLocaleDateString()}</p>
      <p className="m-0 text-content-muted">SHA-256: <Fingerprint sha256={ca.sha256} /></p>
    </>
  );
}
