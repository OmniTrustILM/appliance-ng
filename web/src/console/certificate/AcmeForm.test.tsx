import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { CertificatePage } from './CertificatePage';

const certificatePath = 'GET /appliance/api/v1/certificate';
const optionsPath = 'GET /appliance/api/v1/certificate/acme/ilm-options';
const acmePath = 'PUT /appliance/api/v1/certificate/acme';

const details = {
  subject: 'CN=ilm.example.test', issuer: 'CN=ILM Root CA', dnsNames: ['ilm.example.test'], ipAddresses: ['192.0.2.10'],
  notBefore: new Date(Date.now() - 86400000).toISOString(), notAfter: new Date(Date.now() + 90 * 86400000).toISOString(),
};
const builtin = [200, { source: 'builtin', csrPending: false, certificate: details }] as const;
const options = [200, {
  raProfiles: [{ uuid: 'ra1', name: 'Web servers' }],
  vaultProfiles: [{ uuid: 'vp1', name: 'Appliance vault', vaultUuid: 'v1' }],
}] as const;
const notReady = [409, { code: 'ilm_not_ready', message: 'ILM is not ready yet; try again shortly.' }] as const;

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

async function openForm() {
  render(<CertificatePage />);
  await screen.findByText('CN=ilm.example.test');
  return screen.getByRole('button', { name: 'Use the automatic certificate' }).closest('form') as HTMLElement;
}

describe('AcmeForm', () => {
  it('sends a custom directory with HTTP-01, the binding and a CA bundle', async () => {
    const { sent } = stubApi({ [certificatePath]: builtin, [optionsPath]: notReady, [acmePath]: [202] });
    const form = await openForm();
    const submit = within(form).getByRole('button', { name: 'Use the automatic certificate' });
    expect(submit).toBeDisabled();
    await userEvent.type(within(form).getByLabelText('Directory URL'), 'https://acme.example.com/directory');
    await userEvent.type(within(form).getByLabelText(/^Contact email/), 'ops@example.com');
    await userEvent.type(within(form).getByLabelText(/^External account key ID/), 'kid-1');
    expect(submit).toBeDisabled();
    await userEvent.type(within(form).getByLabelText(/^External account HMAC key/), 'aG1hYw');
    await userEvent.type(within(form).getByLabelText(/^CA bundle/), '-----BEGIN CERTIFICATE-----');
    await userEvent.click(submit);
    expect(await screen.findByText(/within about a minute/i, { selector: 'output *' })).toBeInTheDocument();
    expect(sent[acmePath]).toEqual([{
      directoryUrl: 'https://acme.example.com/directory', email: 'ops@example.com', eabKeyId: 'kid-1', eabHmac: 'aG1hYw',
      caBundle: '-----BEGIN CERTIFICATE-----', solver: { type: 'http01' },
    }]);
  });

  it('sends DNS-01 with the provider settings and credentials', async () => {
    const { sent } = stubApi({ [certificatePath]: builtin, [optionsPath]: notReady, [acmePath]: [202] });
    const form = await openForm();
    await userEvent.type(within(form).getByLabelText('Directory URL'), 'https://acme.example.com/directory');
    await userEvent.click(within(form).getByRole('radio', { name: /DNS-01/ }));
    const submit = within(form).getByRole('button', { name: 'Use the automatic certificate' });
    expect(submit).toBeDisabled();
    await userEvent.selectOptions(within(form).getByLabelText('DNS provider'), 'rfc2136');
    await userEvent.type(within(form).getByLabelText('Name server'), '192.0.2.53');
    await userEvent.type(within(form).getByLabelText(/^TSIG key name/), 'acme');
    await userEvent.type(within(form).getByLabelText(/^TSIG secret/), 'c2VjcmV0');
    await userEvent.click(submit);
    await waitFor(() => expect(sent[acmePath]).toHaveLength(1));
    expect(sent[acmePath][0]).toEqual({
      directoryUrl: 'https://acme.example.com/directory',
      solver: { type: 'dns01', dns01: { provider: 'rfc2136', config: { nameserver: '192.0.2.53', tsigKeyName: 'acme' }, credentials: { tsigSecret: 'c2VjcmV0' } } },
    });
  });

  it('sends the ILM profile with the chosen RA profile and vault profile', async () => {
    const { sent } = stubApi({ [certificatePath]: builtin, [optionsPath]: options, [acmePath]: [202] });
    const form = await openForm();
    const ilm = within(form).getByRole('radio', { name: /ILM on this appliance/ });
    await waitFor(() => expect(ilm).toBeEnabled());
    await userEvent.click(ilm);
    expect(within(form).queryByLabelText('Directory URL')).not.toBeInTheDocument();
    await userEvent.selectOptions(within(form).getByLabelText('RA profile'), 'ra1');
    await userEvent.selectOptions(within(form).getByLabelText('Vault profile'), 'vp1');
    await userEvent.click(within(form).getByRole('button', { name: 'Use the automatic certificate' }));
    await waitFor(() => expect(sent[acmePath]).toHaveLength(1));
    expect(sent[acmePath][0]).toEqual({ ilm: { raProfileUuid: 'ra1', vaultUuid: 'v1', vaultProfileUuid: 'vp1' } });
  });

  it('disables the ILM option with a note while ILM is not ready', async () => {
    stubApi({ [certificatePath]: builtin, [optionsPath]: notReady });
    const form = await openForm();
    await waitFor(() => expect(within(form).getByText('Available once ILM is installed and ready.')).toBeInTheDocument());
    expect(within(form).getByRole('radio', { name: /ILM on this appliance/ })).toBeDisabled();
  });

  it('says that ILM needs a vault profile when it has none', async () => {
    stubApi({ [certificatePath]: builtin, [optionsPath]: [200, { raProfiles: [{ uuid: 'ra1', name: 'Web' }], vaultProfiles: [] }] });
    const form = await openForm();
    const ilm = within(form).getByRole('radio', { name: /ILM on this appliance/ });
    await waitFor(() => expect(ilm).toBeEnabled());
    await userEvent.click(ilm);
    expect(within(form).getByText('ILM needs a vault profile to hold the ACME account key.')).toBeInTheDocument();
    expect(within(form).getByRole('button', { name: 'Use the automatic certificate' })).toBeDisabled();
  });

  it('shows the current settings and why the certificate is not issued yet', async () => {
    stubApi({
      [certificatePath]: [200, {
        source: 'acme', csrPending: false, certificate: details,
        acme: {
          directoryUrl: 'https://acme.example.com/directory', email: 'ops@example.com', eabKeyId: 'kid-1', eabHmacSet: true,
          solver: { type: 'dns01', dns01: { provider: 'cloudflare', config: {}, credentialKeys: ['apiToken'] } },
          lastError: 'the DNS challenge failed; check the DNS provider settings and credentials',
        },
      }],
      [optionsPath]: notReady,
    });
    const form = await openForm();
    expect(within(form).getByText('the DNS challenge failed; check the DNS provider settings and credentials')).toBeInTheDocument();
    expect(within(form).getByText('DNS-01 through Cloudflare')).toBeInTheDocument();
    expect(within(form).getByLabelText('Directory URL')).toHaveValue('https://acme.example.com/directory');
    expect(within(form).getByLabelText('DNS provider')).toHaveValue('cloudflare');
    expect(within(form).getAllByText('It is stored. Enter it again to save the settings.')).toHaveLength(2);
    expect(screen.getByRole('button', { name: 'Go back to the built-in certificate' })).toBeInTheDocument();
  });

  it('keeps stored optional credentials and the CA bundle on a new save', async () => {
    const { sent } = stubApi({
      [certificatePath]: [200, {
        source: 'acme', csrPending: false, certificate: details,
        acme: {
          directoryUrl: 'https://acme.example.com/directory', caBundle: '-----BEGIN CERTIFICATE-----',
          solver: { type: 'dns01', dns01: { provider: 'rfc2136', config: { nameserver: '192.0.2.53' }, credentialKeys: ['tsigSecret'] } },
        },
      }],
      [optionsPath]: notReady, [acmePath]: [202],
    });
    const form = await openForm();
    expect(within(form).getByLabelText(/^CA bundle/)).toHaveValue('-----BEGIN CERTIFICATE-----');
    const submit = within(form).getByRole('button', { name: 'Use the automatic certificate' });
    expect(submit).toBeDisabled();
    await userEvent.type(within(form).getByLabelText(/^TSIG secret/), 'c2VjcmV0');
    await userEvent.click(submit);
    await waitFor(() => expect(sent[acmePath]).toHaveLength(1));
    expect(sent[acmePath][0]).toEqual({
      directoryUrl: 'https://acme.example.com/directory', caBundle: '-----BEGIN CERTIFICATE-----',
      solver: { type: 'dns01', dns01: { provider: 'rfc2136', config: { nameserver: '192.0.2.53' }, credentials: { tsigSecret: 'c2VjcmV0' } } },
    });
  });

  it('shows the ILM profile in use', async () => {
    stubApi({
      [certificatePath]: [200, {
        source: 'acme', csrPending: false, certificate: details,
        acme: { solver: { type: 'http01' }, ilm: { raProfileUuid: 'ra1', vaultUuid: 'v1', vaultProfileUuid: 'vp1' } },
      }],
      [optionsPath]: options,
    });
    const form = await openForm();
    expect(within(form).getByText('ILM on this appliance', { selector: 'dd' })).toBeInTheDocument();
    await waitFor(() => expect(within(form).getByLabelText('RA profile')).toHaveValue('ra1'));
    expect(within(form).getByLabelText('Vault profile')).toHaveValue('vp1');
  });

  it('reports a refused or unreachable request', async () => {
    const { mock } = stubApi({
      [certificatePath]: builtin, [optionsPath]: notReady,
      [acmePath]: [400, { code: 'invalid', message: 'The ACME directory must be an https URL.', field: 'directoryUrl' }],
    });
    const form = await openForm();
    await userEvent.type(within(form).getByLabelText('Directory URL'), 'http://acme.example.com');
    await userEvent.click(within(form).getByRole('button', { name: 'Use the automatic certificate' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('The ACME directory must be an https URL.');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(within(form).getByRole('button', { name: 'Use the automatic certificate' }));
    expect(await within(form).findByText(/cannot be reached/i)).toBeInTheDocument();
  });
});
