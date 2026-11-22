import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { CertificatePage } from './CertificatePage';

const day = 24 * 60 * 60 * 1000;
const details = (expiresInDays: number) => ({
  subject: 'CN=ilm.example.test',
  issuer: 'CN=ILM Root CA',
  dnsNames: ['ilm.example.test', 'node.example.test'],
  ipAddresses: ['192.0.2.10'],
  notBefore: new Date(Date.now() - day).toISOString(),
  notAfter: new Date(Date.now() + expiresInDays * day).toISOString(),
});
const info = (over: object = {}, expiresInDays = 365) =>
  [200, { source: 'secret', csrPending: false, certificate: details(expiresInDays), ...over }] as const;

const pem = (name: string) => new File([`-----BEGIN ${name}-----`], `${name}.pem`, { type: 'application/x-pem-file' });

beforeEach(() => {
  vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: vi.fn(() => 'blob:csr'), revokeObjectURL: vi.fn() }));
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('CertificatePage', () => {
  it('shows the certificate in use', async () => {
    stubApi({ 'GET /appliance/api/v1/certificate': info() });
    render(<CertificatePage />);
    expect(await screen.findByText('CN=ilm.example.test')).toBeInTheDocument();
    expect(screen.getByText('ilm.example.test, node.example.test')).toBeInTheDocument();
    expect(screen.getByText('192.0.2.10')).toBeInTheDocument();
    expect(screen.getByText('CN=ILM Root CA')).toBeInTheDocument();
    expect(screen.getByText('Uploaded certificate')).toBeInTheDocument();
    expect(screen.queryByText(/expires within/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /root certificate/i })).not.toBeInTheDocument();
    expect(screen.getByText(/Without the node.s IP address in the certificate, the console is reachable only by its host name/)).toBeInTheDocument();
  });

  it('warns when the certificate expires within 30 days and when it has expired', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/certificate': info({}, 29) });
    const { unmount } = render(<CertificatePage />);
    expect(await screen.findByText(/expires within 30 days/i)).toBeInTheDocument();
    unmount();
    mock.mockClear();
    stubApi({ 'GET /appliance/api/v1/certificate': info({}, -1) });
    render(<CertificatePage />);
    expect(await screen.findByText(/has expired/i)).toBeInTheDocument();
  });

  it('offers the root certificate with the built-in certificate and says when none exists yet', async () => {
    const { unmount } = render(<div />);
    unmount();
    stubApi({ 'GET /appliance/api/v1/certificate': info({ source: 'builtin' }) });
    const first = render(<CertificatePage />);
    expect(await screen.findByRole('link', { name: /root certificate/i })).toHaveAttribute('href', '/appliance/api/v1/ca.pem');
    expect(screen.queryByRole('button', { name: /back to the built-in/i })).not.toBeInTheDocument();
    first.unmount();
    stubApi({ 'GET /appliance/api/v1/certificate': [200, { source: 'builtin', csrPending: false }] });
    render(<CertificatePage />);
    expect(await screen.findByText(/has not been issued yet/i)).toBeInTheDocument();
  });

  it('reports a certificate that cannot be read', async () => {
    stubApi({ 'GET /appliance/api/v1/certificate': [500, { code: 'internal', message: 'Reading failed.' }] });
    render(<CertificatePage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('Reading failed.');
  });

  it('uploads dropped files as multipart fields', async () => {
    const { sent } = stubApi({ 'GET /appliance/api/v1/certificate': info(), 'PUT /appliance/api/v1/certificate': [202] });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    const submit = screen.getByRole('button', { name: 'Upload the certificate' });
    expect(submit).toBeDisabled();
    const zones = screen.getAllByTestId('drop-zone');
    fireEvent.dragOver(zones[0]);
    fireEvent.dragLeave(zones[0]);
    fireEvent.drop(zones[0], { dataTransfer: { files: [pem('CERTIFICATE')] } });
    fireEvent.drop(zones[1], { dataTransfer: { files: [pem('KEY')] } });
    fireEvent.drop(zones[2], { dataTransfer: { files: [] } });
    expect(await screen.findByText('CERTIFICATE.pem')).toBeInTheDocument();
    await userEvent.click(submit);
    expect(await screen.findByText(/within about a minute/i, { selector: 'output *' })).toBeInTheDocument();
    const [body] = sent['PUT /appliance/api/v1/certificate'] as Record<string, File>[];
    expect(Object.keys(body).sort()).toEqual(['certificate', 'key']);
    expect(await body.certificate.text()).toBe('-----BEGIN CERTIFICATE-----');
    expect(await body.key.text()).toBe('-----BEGIN KEY-----');
  });

  it('uploads files chosen with the file picker, chain included', async () => {
    const { sent } = stubApi({ 'GET /appliance/api/v1/certificate': info(), 'PUT /appliance/api/v1/certificate': [202] });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    await userEvent.upload(screen.getByLabelText(/^Certificate/), pem('CERTIFICATE'));
    await userEvent.upload(screen.getByLabelText(/^Private key/), pem('KEY'));
    await userEvent.upload(screen.getByLabelText(/^Chain/), pem('CHAIN'));
    await userEvent.click(screen.getByRole('button', { name: 'Upload the certificate' }));
    await waitFor(() => expect(sent['PUT /appliance/api/v1/certificate']).toHaveLength(1));
    expect(Object.keys(sent['PUT /appliance/api/v1/certificate'][0] as object).sort()).toEqual(['certificate', 'chain', 'key']);
  });

  it('shows the message of a refused upload and says the current certificate stays', async () => {
    stubApi({
      'GET /appliance/api/v1/certificate': info(),
      'PUT /appliance/api/v1/certificate': [400, { code: 'invalid', message: 'The key does not match the certificate.', field: 'key' }],
    });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    await userEvent.upload(screen.getByLabelText(/^Certificate/), pem('CERTIFICATE'));
    await userEvent.upload(screen.getByLabelText(/^Private key/), pem('KEY'));
    await userEvent.click(screen.getByRole('button', { name: 'Upload the certificate' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('The key does not match the certificate.');
    expect(alert).toHaveTextContent('The current certificate stays in use.');
    expect(screen.getByRole('button', { name: 'Upload the certificate' })).toBeEnabled();
  });

  it('reports an unreachable appliance on upload', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/certificate': info() });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.upload(screen.getByLabelText(/^Certificate/), pem('CERTIFICATE'));
    await userEvent.upload(screen.getByLabelText(/^Private key/), pem('KEY'));
    await userEvent.click(screen.getByRole('button', { name: 'Upload the certificate' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/cannot be reached.*stays in use/i);
  });

  it('downloads the signing request and then takes the signed certificate', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let pending = false;
    const { sent } = stubApi({
      'GET /appliance/api/v1/certificate': () => info({ csrPending: pending }),
      'POST /appliance/api/v1/certificate/csr': () => {
        pending = true;
        return [200, '-----BEGIN CERTIFICATE REQUEST-----'];
      },
      'PUT /appliance/api/v1/certificate/csr/signed': [202],
    });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    expect(screen.queryByText('Upload the signed certificate')).not.toBeInTheDocument();
    expect(screen.queryByText(/replaces the pending one/)).not.toBeInTheDocument();
    expect(screen.getByText(/request names the host names of the node and of ILM, not the node/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Create a signing request' }));
    expect(await screen.findByRole('heading', { name: 'Upload the signed certificate' })).toBeInTheDocument();
    expect(screen.getByText(/replaces the pending one/)).toBeInTheDocument();
    expect(URL.createObjectURL).toHaveBeenCalledTimes(1);
    expect(HTMLAnchorElement.prototype.click).toHaveBeenCalledTimes(1);
    expect(URL.revokeObjectURL).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(10000);
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:csr');

    const form = screen.getByRole('button', { name: 'Upload the signed certificate' }).closest('form') as HTMLElement;
    expect(within(form).queryByLabelText(/^Private key/)).not.toBeInTheDocument();
    await userEvent.upload(within(form).getByLabelText(/^Certificate/), pem('CERTIFICATE'));
    await userEvent.click(within(form).getByRole('button', { name: 'Upload the signed certificate' }));
    await waitFor(() => expect(sent['PUT /appliance/api/v1/certificate/csr/signed']).toHaveLength(1));
    expect(Object.keys(sent['PUT /appliance/api/v1/certificate/csr/signed'][0] as object)).toEqual(['certificate']);
  });

  it('reports a signing request that cannot be created', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/certificate': info(),
      'POST /appliance/api/v1/certificate/csr': [409, { code: 'conflict', message: 'The setup is not complete.' }],
    });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Create a signing request' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('The setup is not complete.');
    expect(URL.createObjectURL).not.toHaveBeenCalled();
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Create a signing request' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('goes back to the built-in certificate after the confirmation', async () => {
    const { sent } = stubApi({ 'GET /appliance/api/v1/certificate': info(), 'PUT /appliance/api/v1/certificate/builtin': [202] });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Go back to the built-in certificate' }));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(sent['PUT /appliance/api/v1/certificate/builtin']).toBeUndefined();
    await userEvent.click(screen.getByRole('button', { name: 'Go back to the built-in certificate' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Use the built-in certificate' }));
    expect(await screen.findByText(/within about a minute/i, { selector: 'output *' })).toBeInTheDocument();
    expect(sent['PUT /appliance/api/v1/certificate/builtin']).toHaveLength(1);
  });

  it('reports a refused or unreachable switch to the built-in certificate', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/certificate': info(),
      'PUT /appliance/api/v1/certificate/builtin': [409, { code: 'conflict', message: 'Not possible now.' }],
    });
    render(<CertificatePage />);
    await screen.findByText('CN=ilm.example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Go back to the built-in certificate' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Use the built-in certificate' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Not possible now.');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Go back to the built-in certificate' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Use the built-in certificate' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });
});
