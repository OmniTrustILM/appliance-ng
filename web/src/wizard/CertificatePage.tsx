import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';

/** Explains the certificate of the ILM; only the built-in authority is available for now. */
export function CertificatePage({ onNext, onBack }: Readonly<{ onNext: () => void; onBack: () => void }>) {
  return (
    <Card
      back={<Button variant="ghost" onClick={onBack}>Back</Button>}
      actions={<Button onClick={onNext}>Continue</Button>}
    >
      <PageHeading
        eyebrow="Platform"
        title="Certificate"
        lead="The appliance issues the certificate of the ILM from its own built-in certificate authority. Uploading a certificate or creating a signing request for your own authority comes later."
      />
      <p>
        Clients trust the ILM once they trust the authority's root certificate. You can download it as soon as the
        installation starts.
      </p>
    </Card>
  );
}
