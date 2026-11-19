import { useState, type SubmitEvent } from 'react';
import { Link } from 'react-router';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { Field } from '../../components/Field';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, unreachable } from '../../setup/failure';
import { NetworkFields, networkProblems, networkValues, PAIR, proxyOf, type NetworkValues } from '../../setup/networkFields';
import { isIpv4, splitList } from '../../setup/validate';
import { useLoaded } from '../shared/useLoaded';

type Proxy = components['schemas']['Proxy'];
type Errors = Partial<Record<string, string>>;

const MONO = 'font-mono';
const LINK = 'text-brand hover:underline';
const proxyFields = new Set(['proxy', 'http', 'https', 'noProxy']);
/** The form fields that show their own error; the server's other fields go to the form message. */
const fieldsWithError = new Set(['gateway', 'dns', 'ntp', 'proxy']);

const loadAppliance = () => api.GET('/api/v1/appliance');

const sameProxy = (a?: Proxy, b?: Proxy) => JSON.stringify(a ?? {}) === JSON.stringify(b ?? {});

/** The form field a server-reported field belongs to; the proxy fields share one message. */
const fieldOf = (field: string) => (proxyFields.has(field) ? 'proxy' : field);

/** Changes the gateway, DNS and NTP servers and the proxy of the node; its host name and address are fixed. */
export function NodePage() {
  const { data: settings, error: loadError } = useLoaded(loadAppliance);
  const [gateway, setGateway] = useState('');
  const [network, setNetwork] = useState<NetworkValues>(networkValues({}));
  const [savedProxy, setSavedProxy] = useState<Proxy>();
  const [errors, setErrors] = useState<Errors>({});
  const [saved, setSaved] = useState<{ proxyChanged: boolean }>();
  const [busy, setBusy] = useState(false);

  // The settings read fill the form.
  const [seen, setSeen] = useState(settings);
  if (settings !== seen) {
    setSeen(settings);
    if (settings) {
      setGateway(settings.node.gateway ?? '');
      setNetwork(networkValues(settings));
      setSavedProxy(settings.proxy);
    }
  }

  function problems(): Errors {
    const found: Errors = { ...networkProblems(network) };
    if (gateway.trim() !== '' && !isIpv4(gateway.trim())) found.gateway = 'Enter the IPv4 address of the gateway.';
    return found;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setSaved(undefined);
    const found = problems();
    setErrors(found);
    if (Object.keys(found).length > 0) return;
    const proxy = proxyOf(network);
    setBusy(true);
    const result = await attempt(() =>
      api.PUT('/api/v1/node', {
        body: {
          gateway: gateway.trim(),
          dns: splitList(network.dns),
          ntp: splitList(network.ntp),
          ...(proxy && { proxy }),
        },
      }),
    );
    setBusy(false);
    if (!result) return setErrors({ form: unreachable });
    const { response, error } = result;
    if (response.ok) {
      setSaved({ proxyChanged: !sameProxy(proxy, savedProxy) });
      return setSavedProxy(proxy);
    }
    const field = error?.field && fieldOf(error.field);
    if (response.status === 400 && field && fieldsWithError.has(field)) return setErrors({ [field]: error.message });
    setErrors({ form: error?.message ?? 'The appliance could not apply the settings.' });
  }

  if (!settings) {
    return (
      <PageWidth>
        <Card>{loadError ? <Alert tone="error">{loadError}</Alert> : <Loading />}</Card>
      </PageWidth>
    );
  }

  const address = settings.node.address;

  return (
    <PageWidth>
      <form onSubmit={(event) => void submit(event)} noValidate>
        <Card actions={<Button type="submit" busy={busy}>Save</Button>}>
          <PageHeading eyebrow="Platform" title="Node" lead="How this appliance is named and reached on your network." />
          <Field
            label="Host name"
            name="hostName"
            value={settings.node.hostName}
            readOnly
            hint={
              <>
                The host name is fixed after the setup. The public name changes with the{' '}
                <Link className={LINK} to="/platform">ILM host name</Link> and the{' '}
                <Link className={LINK} to="/certificate">certificate</Link>.
              </>
            }
          />
          <div className={PAIR}>
            <Field
              label="Address"
              name="address"
              className={MONO}
              value={address || 'Assigned by DHCP'}
              readOnly
              hint="The address is fixed after the setup."
            />
            <Field
              label="Gateway"
              name="gateway"
              className={MONO}
              value={gateway}
              onChange={(e) => setGateway(e.target.value)}
              error={errors.gateway}
              disabled={!address}
              hint={address ? undefined : 'DHCP delivers the gateway with the address.'}
            />
          </div>
          <NetworkFields values={network} errors={errors} onChange={setNetwork} />
          {errors.form && <Alert tone="error">{errors.form}</Alert>}
          {saved && <Alert tone="success">The node settings are saved and are being applied.</Alert>}
          {saved?.proxyChanged && (
            <Alert tone="warning">
              The proxy applies to the host after a restart. <Link className={LINK} to="/system">Restart the node</Link>
            </Alert>
          )}
        </Card>
      </form>
    </PageWidth>
  );
}
