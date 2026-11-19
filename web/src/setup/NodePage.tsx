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
import { NetworkFields, networkProblems, networkValues, PAIR, proxyOf } from './networkFields';
import { isFqdn, isIpv4, isIpv4Cidr, splitList } from './validate';

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
  const [network, setNetwork] = useState(networkValues(initial ?? {}));
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
        setNetwork((current) => ({ ...current, ...networkValues({ dns: values.dns, ntp: values.ntp }) }));
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
    Object.assign(found, networkProblems(network));
    return found;
  }

  function settings(): NodeSettings {
    const proxy = proxyOf(network);
    return {
      hostName: hostName.trim().toLowerCase(),
      address: mode === 'static' ? address.trim() : '',
      gateway: mode === 'static' ? gateway.trim() : '',
      dns: splitList(network.dns),
      ntp: splitList(network.ntp),
      ...(proxy && { proxy }),
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
        <NetworkFields values={network} errors={errors} onChange={setNetwork} />
        {errors.form && <Alert tone="error">{errors.form}</Alert>}
      </Card>
    </form>
  );
}
