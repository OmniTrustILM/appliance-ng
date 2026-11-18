/** Downloads the root certificate of the built-in authority; it is public and exists once the certificate is issued. */
export function RootCaLink() {
  return <a href="/api/v1/ca.pem" download>Download the root certificate</a>;
}
