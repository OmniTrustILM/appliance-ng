/**
 * The node installs and reboots; the setup service stops seconds after the apply, so the page asks for nothing
 * and leaves the reload to the user.
 */
export function ApplyingPage({ address }: { address?: string }) {
  const url = address ? `https://${address}:8443/` : undefined;
  return (
    <div>
      <h1>Installing</h1>
      <p>The node installs and reboots now. Reload this page in about five minutes at</p>
      {url ? (
        <p><a href={url}>{url}</a></p>
      ) : (
        <p>the address shown on the appliance console.</p>
      )}
      <p>This page does not update by itself.</p>
    </div>
  );
}
