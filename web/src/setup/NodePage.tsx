import { useEffect, useState, type SubmitEvent } from 'react';
import { api } from '../api/client';
import type { components } from '../api/schema';
import { Alert } from '../components/Alert';
import { Button } from '../components/Button';
import { Card } from '../components/Card';
import { ChoiceField } from '../components/ChoiceField';
import { Field } from '../components/Field';
import { PageHeading } from '../components/PageHeading';
import { Loading } from '../components/Spinner';
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

const MONO = 'font-mono';
const PAIR = 'grid gap-x-4 sm:grid-cols-2';

const withoutPrefix = (cidr?: string) => cidr?.split('/')[0];

/** Asks for the host name, the network settings and the optional proxy of the node. */
export function NodePage({ initial, onDone, onExpired }: Readonly<Props>) {
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

  async function submit(event: SubmitEvent) {
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

  if (!loaded) return <Loading />;

  return (
    <form onSubmit={submit} noValidate>
      <Card actions={<Button type="submit" busy={busy}>Continue</Button>}>
        <PageHeading eyebrow="Setup" title="Node settings" lead="How this appliance is named and reached on your network." />
        <Field label="Host name" name="hostName" value={hostName} onChange={(e) => setHostName(e.target.value)} error={errors.hostName} />
        <fieldset className="mb-[18px]">
          <legend className="mb-2 text-sm font-medium">Address</legend>
          <ChoiceField type="radio" name="mode" label="Keep the address from DHCP" checked={mode === 'dhcp'} onChange={() => chooseMode('dhcp')} />
          <ChoiceField type="radio" name="mode" label="Use a static address" checked={mode === 'static'} onChange={() => chooseMode('static')} />
          {mode === 'dhcp' && (
            <output className="mt-3 block rounded-md border border-warning/35 bg-warning-surface px-3.5 py-3 text-sm text-content">
              Reserve {dhcp.address ?? 'the address of the node'} for the node in the DHCP server. If the address
              changes after the installation, the console is no longer reachable at the address shown here.
            </output>
          )}
          {mode === 'static' && (
            <div className={PAIR}>
              <Field label="Address (CIDR)" name="address" className={MONO} value={address} onChange={(e) => setAddress(e.target.value)} error={errors.address} placeholder="192.0.2.10/24" />
              <Field label="Gateway" name="gateway" className={MONO} value={gateway} onChange={(e) => setGateway(e.target.value)} error={errors.gateway} />
            </div>
          )}
        </fieldset>
        <div className={PAIR}>
          <Field label="DNS servers" name="dns" className={MONO} value={dns} onChange={(e) => setDns(e.target.value)} error={errors.dns} placeholder="comma-separated" />
          <Field label="NTP servers" name="ntp" className={MONO} value={ntp} onChange={(e) => setNtp(e.target.value)} error={errors.ntp} placeholder="comma-separated; empty uses the default" />
        </div>
        <fieldset className="mt-7 border-t border-divider pt-5">
          <legend className="float-left mb-3.5 w-full text-sm font-semibold">Proxy (optional)</legend>
          <div className={`clear-both ${PAIR}`}>
            <Field label="HTTP proxy" name="http" className={MONO} value={http} onChange={(e) => setHttp(e.target.value)} error={errors.proxy} placeholder="http://proxy.example.com:3128" />
            <Field label="HTTPS proxy" name="https" className={MONO} value={https} onChange={(e) => setHttps(e.target.value)} />
          </div>
          <Field label="Hosts that bypass the proxy" name="noProxy" className={MONO} value={noProxy} onChange={(e) => setNoProxy(e.target.value)} placeholder="comma-separated" />
        </fieldset>
        {errors.form && <Alert tone="error">{errors.form}</Alert>}
      </Card>
    </form>
  );
}
