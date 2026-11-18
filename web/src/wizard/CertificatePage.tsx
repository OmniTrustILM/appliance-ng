/** Explains the certificate of the ILM; only the built-in authority is available for now. */
export function CertificatePage({ onNext, onBack }: { onNext: () => void; onBack: () => void }) {
  return (
    <div>
      <h1>Certificate</h1>
      <p>
        The appliance issues the certificate of the ILM from its own built-in certificate authority. Uploading a
        certificate or creating a signing request for your own authority comes later.
      </p>
      <p>
        Clients trust the ILM once they trust the authority's root certificate. You can download it as soon as the
        installation starts.
      </p>
      <button type="button" onClick={onBack}>Back</button>
      <button type="button" onClick={onNext}>Continue</button>
    </div>
  );
}
