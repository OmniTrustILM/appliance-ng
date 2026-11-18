import { Card } from '../components/Card';
import { PageHeading } from '../components/PageHeading';

/**
 * The node installs and reboots; the setup service stops seconds after the apply, so the page asks for nothing
 * and leaves the reload to the user.
 */
export function ApplyingPage({ address }: Readonly<{ address?: string }>) {
  const url = address ? `https://${address}:8443/` : undefined;
  return (
    <Card>
      <PageHeading eyebrow="Setup" title="Installing" />
      <p className="mb-2">The node installs and reboots now. Reload this page in about five minutes at</p>
      {url ? (
        <p className="mb-2">
          <a href={url} className="break-all font-mono text-brand underline">{url}</a>
        </p>
      ) : (
        <p className="mb-2">the address shown on the appliance console.</p>
      )}
      <p className="text-content-muted">This page does not update by itself.</p>
    </Card>
  );
}
