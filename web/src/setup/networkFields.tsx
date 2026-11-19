import type { components } from '../api/schema';
import { Field } from '../components/Field';
import { isHostOrAddress, isIpAddress, isProxyUrl, splitList } from './validate';

type Proxy = components['schemas']['Proxy'];

/** The DNS, NTP and proxy settings as the user types them. */
export type NetworkValues = { dns: string; ntp: string; http: string; https: string; noProxy: string };
export type NetworkErrors = Partial<Record<'dns' | 'ntp' | 'proxy', string>>;

const MONO = 'font-mono';
export const PAIR = 'grid gap-x-4 sm:grid-cols-2';

/** The values of existing DNS, NTP and proxy settings, as typed lists. */
export function networkValues(source: { dns?: string[]; ntp?: string[]; proxy?: Proxy }): NetworkValues {
  return {
    dns: (source.dns ?? []).join(', '),
    ntp: (source.ntp ?? []).join(', '),
    http: source.proxy?.http ?? '',
    https: source.proxy?.https ?? '',
    noProxy: source.proxy?.noProxy ?? '',
  };
}

/** The problems the server would report for the DNS, NTP and proxy settings, by field. */
export function networkProblems({ dns, ntp, http, https }: NetworkValues): NetworkErrors {
  const found: NetworkErrors = {};
  const dnsServers = splitList(dns);
  if (dnsServers.length === 0) found.dns = 'Enter at least one DNS server.';
  else if (!dnsServers.every(isIpAddress)) found.dns = 'Enter IP addresses separated by commas.';
  if (!splitList(ntp).every(isHostOrAddress)) found.ntp = 'Enter host names or IP addresses separated by commas.';
  if ([http, https].some((url) => url.trim() !== '' && !isProxyUrl(url.trim()))) {
    found.proxy = 'Enter proxy URLs that start with http:// or https://.';
  }
  return found;
}

/** The proxy to send, without empty entries; undefined when none is set. */
export function proxyOf({ http, https, noProxy }: NetworkValues): Proxy | undefined {
  const proxy = {
    ...(http.trim() && { http: http.trim() }),
    ...(https.trim() && { https: https.trim() }),
    ...(noProxy.trim() && { noProxy: noProxy.trim() }),
  };
  return Object.keys(proxy).length > 0 ? proxy : undefined;
}

type NetworkFieldsProps = Readonly<{
  values: NetworkValues;
  errors: NetworkErrors;
  onChange: (values: NetworkValues) => void;
}>;

/** The DNS and NTP server fields and the optional proxy group. */
export function NetworkFields({ values, errors, onChange }: NetworkFieldsProps) {
  const bind = (name: keyof NetworkValues) => ({
    name,
    className: MONO,
    value: values[name],
    onChange: (event: { target: { value: string } }) => onChange({ ...values, [name]: event.target.value }),
  });
  return (
    <>
      <div className={PAIR}>
        <Field label="DNS servers" error={errors.dns} placeholder="comma-separated" {...bind('dns')} />
        <Field label="NTP servers" error={errors.ntp} placeholder="comma-separated; empty uses the default" {...bind('ntp')} />
      </div>
      <fieldset className="mt-7 border-t border-divider pt-5">
        <legend className="float-left mb-3.5 w-full text-sm font-semibold">Proxy (optional)</legend>
        <div className={`clear-both ${PAIR}`}>
          <Field label="HTTP proxy" error={errors.proxy} placeholder="http://proxy.example.com:3128" {...bind('http')} />
          <Field label="HTTPS proxy" {...bind('https')} />
        </div>
        <Field label="Hosts that bypass the proxy" placeholder="comma-separated" {...bind('noProxy')} />
      </fieldset>
    </>
  );
}
