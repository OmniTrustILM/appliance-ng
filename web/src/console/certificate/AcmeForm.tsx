import { useId, useState, type SubmitEvent } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { ChoiceField } from '../../components/ChoiceField';
import { Field, INPUT } from '../../components/Field';
import { SummaryRow } from '../../components/SummaryRow';
import { TextArea } from '../../components/TextArea';
import { attempt, unreachable } from '../../setup/failure';
import { useLoaded } from '../shared/useLoaded';
import { dnsProviders, type DnsKey } from './dns01';

type AcmeInfo = components['schemas']['AcmeInfo'];
type AcmeSettings = components['schemas']['AcmeSettings'];
type Mode = 'custom' | 'ilm';
type Solver = 'http01' | 'dns01';

const loadIlmOptions = () => api.GET('/certificate/acme/ilm-options');
const enterAgain = 'It is stored. Enter it again to save the settings.';

/** The ACME settings in use, with the reason the certificate is not issued yet. */
function CurrentAcme({ acme }: Readonly<{ acme: AcmeInfo }>) {
  const dns = acme.solver.dns01;
  return (
    <>
      {acme.lastError && <Alert tone="warning" title="The certificate is not issued yet">{acme.lastError}</Alert>}
      <dl className="m-0 mb-5">
        <SummaryRow term="ACME server">{acme.ilm ? 'ILM on this appliance' : acme.directoryUrl}</SummaryRow>
        {acme.email && <SummaryRow term="Contact email">{acme.email}</SummaryRow>}
        {!acme.ilm && (
          <SummaryRow term="Challenge">
            {dns ? `DNS-01 through ${dnsProviders[dns.provider]?.label ?? dns.provider}` : 'HTTP-01'}
          </SummaryRow>
        )}
      </dl>
    </>
  );
}

type AcmeFormProps = Readonly<{ acme?: AcmeInfo; onStored: () => void }>;

/**
 * Selects a certificate from an ACME server: one of your own, with an HTTP-01 or DNS-01 challenge, or the ACME
 * profile of ILM on this appliance, offered once ILM answers.
 */
export function AcmeForm({ acme, onStored }: AcmeFormProps) {
  const ids = { provider: useId(), raProfile: useId(), vaultProfile: useId() };
  const { data: options } = useLoaded(loadIlmOptions);
  const [mode, setMode] = useState<Mode>(acme?.ilm ? 'ilm' : 'custom');
  const [text, setText] = useState({
    directoryUrl: acme?.directoryUrl ?? '', email: acme?.email ?? '', eabKeyId: acme?.eabKeyId ?? '', eabHmac: '', caBundle: acme?.caBundle ?? '',
  });
  const [solver, setSolver] = useState<Solver>(acme?.solver.type ?? 'http01');
  const [provider, setProvider] = useState(acme?.solver.dns01?.provider ?? '');
  const [config, setConfig] = useState<Record<string, string>>(acme?.solver.dns01?.config ?? {});
  const [credentials, setCredentials] = useState<Record<string, string>>({});
  const [raProfile, setRaProfile] = useState(acme?.ilm?.raProfileUuid ?? '');
  const [vaultProfile, setVaultProfile] = useState(acme?.ilm?.vaultProfileUuid ?? '');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const dns = solver === 'dns01' ? dnsProviders[provider] : undefined;
  const stored = acme?.solver.dns01?.provider === provider ? acme.solver.dns01.credentialKeys : [];
  const vault = options?.vaultProfiles.find((p) => p.uuid === vaultProfile);
  // A stored credential counts as required until it is entered again, so that saving does not drop it.
  const missing = (keys: readonly DnsKey[], values: Record<string, string>) =>
    keys.some((k) => (k.required || stored.includes(k.name)) && !values[k.name]);
  const complete = mode === 'ilm'
    ? Boolean(options && raProfile && vault)
    : Boolean(text.directoryUrl) && Boolean(text.eabKeyId) === Boolean(text.eabHmac) &&
      (solver === 'http01' || (dns !== undefined && !missing(dns.config, config) && !missing(dns.secrets, credentials)));

  function settings(): AcmeSettings {
    if (mode === 'ilm') {
      return { ilm: { raProfileUuid: raProfile, vaultUuid: vault?.vaultUuid ?? '', vaultProfileUuid: vaultProfile } };
    }
    const given = (value: string) => value || undefined;
    const only = (keys: readonly DnsKey[], values: Record<string, string>) =>
      Object.fromEntries(keys.filter((k) => values[k.name]).map((k) => [k.name, values[k.name]]));
    return {
      directoryUrl: text.directoryUrl, email: given(text.email), eabKeyId: given(text.eabKeyId), eabHmac: given(text.eabHmac),
      caBundle: given(text.caBundle),
      solver: dns
        ? { type: 'dns01', dns01: { provider, config: only(dns.config, config), credentials: only(dns.secrets, credentials) } }
        : { type: 'http01' },
    };
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!complete) return;
    setBusy(true);
    setError(undefined);
    const result = await attempt(() => api.PUT('/certificate/acme', { body: settings() }));
    setBusy(false);
    if (result?.response.ok) return onStored();
    setError(result ? (result.error?.message ?? 'The appliance could not store the settings.') : unreachable);
  }

  const field = (key: keyof typeof text, label: string, extra: Partial<Parameters<typeof Field>[0]> = {}) => (
    <Field label={label} name={key} value={text[key]} autoComplete="off" spellCheck={false}
      onChange={(event) => setText({ ...text, [key]: event.target.value })} {...extra} />
  );
  const dnsField = (k: DnsKey, values: Record<string, string>, set: (v: Record<string, string>) => void, secret: boolean) => (
    <Field key={k.name} label={k.required ? k.label : `${k.label} (optional)`} name={k.name} value={values[k.name] ?? ''}
      type={secret ? 'password' : 'text'} autoComplete="off" spellCheck={false}
      hint={secret && stored.includes(k.name) ? enterAgain : undefined}
      onChange={(event) => set({ ...values, [k.name]: event.target.value })} />
  );

  return (
    <form onSubmit={(event) => void submit(event)} noValidate>
      {acme && <CurrentAcme acme={acme} />}
      {error && <Alert tone="error">{error}</Alert>}
      <fieldset className="mb-[18px]">
        <legend className="mb-1.5 text-sm font-medium">ACME server</legend>
        <ChoiceField type="radio" name="mode" label="An ACME server of your own" checked={mode === 'custom'}
          description="A public or private certificate authority that speaks ACME." onChange={() => setMode('custom')} />
        <ChoiceField type="radio" name="mode" label="ILM on this appliance" checked={mode === 'ilm'} disabled={!options}
          description={options ? 'ILM issues the certificate through an RA profile.' : 'Available once ILM is installed and ready.'}
          onChange={() => setMode('ilm')} />
      </fieldset>

      {mode === 'custom' && (
        <>
          {field('directoryUrl', 'Directory URL', { required: true, placeholder: 'https://acme.example.com/directory' })}
          {field('email', 'Contact email (optional)', { type: 'email' })}
          {field('eabKeyId', 'External account key ID (optional)', { hint: 'Some servers require an external account binding.' })}
          {field('eabHmac', 'External account HMAC key (optional)', {
            type: 'password', hint: acme?.eabHmacSet ? enterAgain : 'Needed with the key ID.',
          })}
          <TextArea label="CA bundle (optional)" name="caBundle" value={text.caBundle}
            onChange={(caBundle) => setText({ ...text, caBundle })}
            hint="The PEM certificates that sign the server's certificate, when it is not publicly trusted." />
          <fieldset className="mb-[18px]">
            <legend className="mb-1.5 text-sm font-medium">Challenge</legend>
            <ChoiceField type="radio" name="solver" label="HTTP-01" checked={solver === 'http01'} onChange={() => setSolver('http01')}
              description="The host names must resolve to this appliance, and port 80 must be reachable from the ACME server." />
            <ChoiceField type="radio" name="solver" label="DNS-01" checked={solver === 'dns01'} onChange={() => setSolver('dns01')}
              description="The appliance proves the host names through your DNS provider." />
          </fieldset>
          {solver === 'dns01' && (
            <>
              <div className="mb-[18px]">
                <label htmlFor={ids.provider} className="mb-1.5 block text-sm font-medium">DNS provider</label>
                <select id={ids.provider} name="provider" value={provider} className={INPUT}
                  onChange={(event) => setProvider(event.target.value)}>
                  <option value="">Choose a provider</option>
                  {Object.entries(dnsProviders).map(([name, p]) => <option key={name} value={name}>{p.label}</option>)}
                </select>
              </div>
              {dns?.config.map((k) => dnsField(k, config, setConfig, false))}
              {dns?.secrets.map((k) => dnsField(k, credentials, setCredentials, true))}
            </>
          )}
        </>
      )}

      {mode === 'ilm' && options && (
        <>
          <div className="mb-[18px]">
            <label htmlFor={ids.raProfile} className="mb-1.5 block text-sm font-medium">RA profile</label>
            <select id={ids.raProfile} name="raProfile" value={raProfile} className={INPUT}
              onChange={(event) => setRaProfile(event.target.value)}>
              <option value="">Choose an RA profile</option>
              {options.raProfiles.map((p) => <option key={p.uuid} value={p.uuid}>{p.name}</option>)}
            </select>
          </div>
          <div className="mb-[18px]">
            <label htmlFor={ids.vaultProfile} className="mb-1.5 block text-sm font-medium">Vault profile</label>
            <select id={ids.vaultProfile} name="vaultProfile" value={vaultProfile} className={INPUT}
              onChange={(event) => setVaultProfile(event.target.value)}>
              <option value="">Choose a vault profile</option>
              {options.vaultProfiles.map((p) => <option key={p.uuid} value={p.uuid}>{p.name}</option>)}
            </select>
            <p className="mt-1 text-[13px] text-content-subtle">It holds the key of the ACME account.</p>
          </div>
          {options.vaultProfiles.length === 0 && <Alert tone="info">ILM needs a vault profile to hold the ACME account key.</Alert>}
        </>
      )}

      <Button type="submit" busy={busy} disabled={!complete}>Use the automatic certificate</Button>
    </form>
  );
}
