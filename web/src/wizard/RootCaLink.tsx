import { apiBase } from '../base';

/** Downloads the root certificate of the built-in authority; it is public and exists once the certificate is issued. */
export function RootCaLink() {
  return <a href={`${apiBase}/ca.pem`} download className="font-medium text-brand underline">Download the root certificate</a>;
}
