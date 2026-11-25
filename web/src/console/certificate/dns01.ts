/** A setting of a DNS provider; a required one must be given. */
export type DnsKey = Readonly<{ name: string; label: string; required?: boolean }>;

/** A DNS provider of the DNS-01 challenge: its plain settings and its credentials, which are stored as secrets. */
export type DnsProvider = Readonly<{ label: string; config: readonly DnsKey[]; secrets: readonly DnsKey[] }>;

const required = (name: string, label: string): DnsKey => ({ name, label, required: true });
const optional = (name: string, label: string): DnsKey => ({ name, label });

/** The DNS providers the appliance supports, by the name the API takes, with the keys the API accepts. */
export const dnsProviders: Readonly<Record<string, DnsProvider>> = {
  rfc2136: {
    label: 'RFC 2136 (BIND and others)',
    config: [required('nameserver', 'Name server'), optional('tsigKeyName', 'TSIG key name'), optional('tsigAlgorithm', 'TSIG algorithm')],
    secrets: [optional('tsigSecret', 'TSIG secret')],
  },
  cloudflare: { label: 'Cloudflare', config: [], secrets: [required('apiToken', 'API token')] },
  route53: {
    label: 'Amazon Route 53',
    config: [optional('region', 'Region'), required('accessKeyID', 'Access key ID'), optional('hostedZoneID', 'Hosted zone ID'), optional('role', 'Role')],
    secrets: [required('secretAccessKey', 'Secret access key')],
  },
  azuredns: {
    label: 'Azure DNS',
    config: [required('subscriptionID', 'Subscription ID'), required('resourceGroupName', 'Resource group'), optional('tenantID', 'Tenant ID'),
      optional('hostedZoneName', 'Hosted zone'), optional('clientID', 'Client ID')],
    secrets: [optional('clientSecret', 'Client secret')],
  },
  clouddns: {
    label: 'Google Cloud DNS',
    config: [required('project', 'Project')],
    secrets: [optional('serviceAccountKey', 'Service account key (JSON)')],
  },
  digitalocean: { label: 'DigitalOcean', config: [], secrets: [required('token', 'API token')] },
  acmedns: { label: 'ACME-DNS', config: [required('host', 'Server URL')], secrets: [required('accountJSON', 'Account (JSON)')] },
  akamai: {
    label: 'Akamai',
    config: [required('serviceConsumerDomain', 'Service consumer domain')],
    secrets: [required('clientToken', 'Client token'), required('clientSecret', 'Client secret'), required('accessToken', 'Access token')],
  },
};
