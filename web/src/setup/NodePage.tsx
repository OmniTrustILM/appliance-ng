import { useEffect, useState, type FormEvent } from 'react';
import { api } from '../api/client';
import type { components } from '../api/schema';
import { Field } from '../components/Field';
import { attempt, failureMessage, unreachable } from './failure';
import { isFqdn, isHostOrAddress, isIpAddress, isIpv4, isIpv4Cidr, isProxyUrl, splitList } from './validate';

export type NodeSettings = components['schemas']['NodeSettings'];
type Errors = Partial<Record<string, string>>;
type Mode = 'dhcp' | 'static';

type Props = {
  /** The answers saved earlier, when the page is opened again from the review. */
  initial?: NodeSettings;
  /** Called with the saved settings and the address the node will have (without the prefix), when known. */
  onDone: (node: NodeSettings, address?: string) => void;
  onExpired: () => void;
};

const withoutPrefix = (cidr?: string) => cidr?.split('/')[0];

/** Asks for the host name, the network settings and the optional proxy of the node. */
export function NodePage({ initial, onDone, onExpired }: Props) {
  const [loaded, setLoaded] = useState(false);
  const [dhcpAddress, setDhcpAddress] = useState<string>();
  const [dhcp, setDhcp] = useState<components['schemas']['DhcpSettings']>({});
  const [mode, setMode] = useState<Mode>(initial?.address ? 'static' : 'dhcp');
  const [hostName, setHostName] = useState(initial?.hostName ?? '');
  const [address, setAddress] = useState(initial?.address ?? '');
  const [gateway, setGateway] = useState(initial?.gateway ?? '');
  const [dns, setDns] = useState((initial?.dns ?? []).join(', '));
  const [ntp, setNtp] = useState((initial?.ntp ?? []).join(', '));
  const [http, setHttp] = useState(initial?.proxy?.http ?? '');
  const [https, setHttps] = useState(initial?.proxy?.https ?? '');
  const [noProxy, setNoProxy] = useState(initial?.proxy?.noProxy ?? '');
  const [errors, setErrors] = useState<Errors>({});
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let active = true;
    void attempt(() => api.GET('/api/v1/setup/dhcp')).then((result) => {
      if (!active) return;
      if (result?.response.status === 401) return onExpired();
      const values = result?.data ?? {};
      setDhcp(values);
      setDhcpAddress(withoutPrefix(values.address));
      if (!initial) {
        setHostName(values.hostName ?? '');
        setDns((values.dns ?? []).join(', '));
        setNtp((values.ntp ?? []).join(', '));
      }
      if (!result) setErrors({ form: unreachable });
      setLoaded(true);
    });
    return () => {
      active = false;
    };
  }, [initial, onExpired]);

  function chooseMode(next: Mode) {
    setMode(next);
    if (next === 'static') {
      setAddress((current) => current || dhcp.address || '');
      setGateway((current) => current || dhcp.gateway || '');
    }
  }

  function problems(): Errors {
    const found: Errors = {};
    if (!isFqdn(hostName.trim())) found.hostName = 'Enter a fully qualified host name, for example node1.example.com.';
    if (mode === 'static') {
      if (!isIpv4Cidr(address.trim())) found.address = 'Enter an IPv4 address in CIDR notation, for example 192.0.2.10/24.';
      if (!isIpv4(gateway.trim())) found.gateway = 'Enter the IPv4 address of the gateway.';
    }
    const dnsServers = splitList(dns);
    if (dnsServers.length === 0) found.dns = 'Enter at least one DNS server.';
    else if (!dnsServers.every(isIpAddress)) found.dns = 'Enter IP addresses separated by commas.';
    if (!splitList(ntp).every(isHostOrAddress)) found.ntp = 'Enter host names or IP addresses separated by commas.';
    if ([http, https].some((url) => url.trim() !== '' && !isProxyUrl(url.trim()))) {
      found.proxy = 'Enter proxy URLs that start with http:// or https://.';
    }
    return found;
  }

  function settings(): NodeSettings {
    const proxy = {
      ...(http.trim() && { http: http.trim() }),
      ...(https.trim() && { https: https.trim() }),
      ...(noProxy.trim() && { noProxy: noProxy.trim() }),
    };
    return {
      hostName: hostName.trim().toLowerCase(),
      address: mode === 'static' ? address.trim() : '',
      gateway: mode === 'static' ? gateway.trim() : '',
      dns: splitList(dns),
      ntp: splitList(ntp),
      ...(Object.keys(proxy).length > 0 && { proxy }),
    };
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    const found = problems();
    setErrors(found);
    if (Object.keys(found).length > 0) return;
    const node = settings();
    setBusy(true);
    const result = await attempt(() => api.PUT('/api/v1/setup/node', { body: node }));
    setBusy(false);
    if (!result) return setErrors({ form: unreachable });
    const { response, error } = result;
    if (response.ok) return onDone(node, withoutPrefix(node.address) || dhcpAddress);
    if (response.status === 401) return onExpired();
    if (response.status === 400 && error && 'field' in error && error.field) {
      return setErrors({ [error.field]: error.message });
    }
    setErrors({ form: failureMessage(response.status) });
  }

  if (!loaded) return <p>Loading…</p>;

  return (
    <form onSubmit={submit} noValidate>
      <h1>Node settings</h1>
      <Field label="Host name" name="hostName" value={hostName} onChange={(e) => setHostName(e.target.value)} error={errors.hostName} />
      <fieldset>
        <legend>Address</legend>
        <label>
          <input type="radio" name="mode" checked={mode === 'dhcp'} onChange={() => chooseMode('dhcp')} />
          Keep the address from DHCP
        </label>
        <label>
          <input type="radio" name="mode" checked={mode === 'static'} onChange={() => chooseMode('static')} />
          Use a static address
        </label>
        {mode === 'dhcp' && (
          <p role="status" className="form-warning">
            Reserve {dhcp.address ?? 'the address of the node'} for the node in the DHCP server. If the address
            changes after the installation, the console is no longer reachable at the address shown here.
          </p>
        )}
        {mode === 'static' && (
          <>
            <Field label="Address (CIDR)" name="address" value={address} onChange={(e) => setAddress(e.target.value)} error={errors.address} placeholder="192.0.2.10/24" />
            <Field label="Gateway" name="gateway" value={gateway} onChange={(e) => setGateway(e.target.value)} error={errors.gateway} />
          </>
        )}
      </fieldset>
      <Field label="DNS servers" name="dns" value={dns} onChange={(e) => setDns(e.target.value)} error={errors.dns} placeholder="comma-separated" />
      <Field label="NTP servers" name="ntp" value={ntp} onChange={(e) => setNtp(e.target.value)} error={errors.ntp} placeholder="comma-separated; empty uses the default" />
      <fieldset>
        <legend>Proxy (optional)</legend>
        <Field label="HTTP proxy" name="http" value={http} onChange={(e) => setHttp(e.target.value)} error={errors.proxy} placeholder="http://proxy.example.com:3128" />
        <Field label="HTTPS proxy" name="https" value={https} onChange={(e) => setHttps(e.target.value)} />
        <Field label="Hosts that bypass the proxy" name="noProxy" value={noProxy} onChange={(e) => setNoProxy(e.target.value)} placeholder="comma-separated" />
      </fieldset>
      {errors.form && <p className="form-error" role="alert">{errors.form}</p>}
      <button type="submit" disabled={busy}>Continue</button>
    </form>
  );
}
