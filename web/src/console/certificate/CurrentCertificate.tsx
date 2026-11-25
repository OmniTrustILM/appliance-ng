import { useState } from 'react';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { SummaryRow } from '../../components/SummaryRow';
import { expiryNotice } from './expiry';

export type CertificateInfo = components['schemas']['CertificateInfo'];

const sources = { builtin: 'Built-in certificate authority', secret: 'Uploaded certificate', acme: 'Automatic certificate (ACME)' } as const;

/** The certificate the appliance serves: subject, names, issuer, validity and source, with a warning near its expiry. */
export function CurrentCertificate({ info }: Readonly<{ info: CertificateInfo }>) {
  const [now] = useState(Date.now);
  const { certificate } = info;
  if (!certificate) {
    return <p className="text-content-muted">The built-in certificate has not been issued yet.</p>;
  }
  const notice = expiryNotice(new Date(certificate.notAfter), now);
  return (
    <>
      {notice && <Alert tone="warning">{notice}</Alert>}
      <dl className="m-0">
        <SummaryRow term="Source">{sources[info.source]}</SummaryRow>
        <SummaryRow term="Subject">{certificate.subject}</SummaryRow>
        <SummaryRow term="Host names">{certificate.dnsNames.join(', ')}</SummaryRow>
        <SummaryRow term="IP addresses">{certificate.ipAddresses.join(', ')}</SummaryRow>
        <SummaryRow term="Issuer">{certificate.issuer}</SummaryRow>
        <SummaryRow term="Valid from">{new Date(certificate.notBefore).toLocaleString()}</SummaryRow>
        <SummaryRow term="Expires">{new Date(certificate.notAfter).toLocaleString()}</SummaryRow>
      </dl>
      {info.csrPending && <p className="mt-3"><Badge tone="wait">Signing request pending</Badge></p>}
    </>
  );
}
