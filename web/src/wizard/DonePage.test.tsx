import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setCsrfToken } from '../api/client';
import { stubApi } from '../setup/test-utils';
import { DonePage } from './DonePage';

const appliance = {
  node: { hostName: 'n' }, dns: [], ntp: [], tlsSource: 'builtin', trustSet: false, registries: [],
  platform: { hostName: 'ilm.example.test', components: [] },
};
const click = vi.fn();

beforeEach(() => {
  URL.createObjectURL = vi.fn(() => 'blob:p12');
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    click(this.download, this.href, this.isConnected, vi.mocked(URL.revokeObjectURL).mock.calls.length);
  });
  setCsrfToken('token-3');
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  click.mockReset();
  setCsrfToken(undefined);
});

function renderPage() {
  return render(<MemoryRouter><DonePage /></MemoryRouter>);
}

async function ask(password: string) {
  await userEvent.type(await screen.findByLabelText('Certificate password'), password);
  await userEvent.click(screen.getByRole('button', { name: 'Download' }));
}

const ADMIN_PASSWORD = 'POST /appliance/api/v1/platform/admin-password';

describe('DonePage', () => {
  it('shows the ILM administrator password once, with a copy button', async () => {
    const writeText = vi.fn(async () => undefined);
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    const { sent } = stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      [ADMIN_PASSWORD]: [200, { user: 'admin', password: 'Generated-pw-1' }],
    });
    renderPage();
    expect(await screen.findByText(/^ILM administrator:/)).toHaveTextContent('ILM administrator: admin / Generated-pw-1');
    expect(screen.getByText('It is not shown again.')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith('Generated-pw-1');
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(sent[ADMIN_PASSWORD]).toHaveLength(1);
  });

  it('says the ILM administrator password was shown already', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      [ADMIN_PASSWORD]: [410, { code: 'shown', message: 'x' }],
    });
    renderPage();
    expect(await screen.findByText(/password was shown already/)).toHaveTextContent('Set a new one on the Components page');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('asks for a reload when the ILM administrator password could not be shown', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      [ADMIN_PASSWORD]: [500, { code: 'internal', message: 'x' }],
    });
    renderPage();
    expect(await screen.findByText('The ILM administrator password could not be shown. Reload the page to try again.')).toBeInTheDocument();
  });

  it('shows no ILM administrator password with an external identity provider', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      [ADMIN_PASSWORD]: [409, { code: 'external_identity', message: 'x' }],
    });
    renderPage();
    await screen.findByRole('link', { name: 'https://ilm.example.test/' });
    expect(screen.queryByText(/ILM administrator/)).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows the ILM address and the root CA', async () => {
    stubApi({ 'GET /appliance/api/v1/appliance': [200, appliance] });
    renderPage();
    expect(await screen.findByRole('link', { name: 'https://ilm.example.test/' })).toHaveAttribute('href', 'https://ilm.example.test/');
    expect(screen.getByRole('link', { name: /root certificate/i })).toHaveAttribute('href', '/appliance/api/v1/ca.pem');
  });

  it('downloads the administrator credential once and forgets the password', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/appliance': [200, appliance] });
    renderPage();
    await screen.findByLabelText('Certificate password');
    mock.mockImplementation(async (request: Request) => {
      const key = `${request.method} ${new URL(request.url).pathname}`;
      if (key === 'POST /appliance/api/v1/platform/admin-credential') {
        expect(await request.json()).toEqual({ password: 'a long enough secret', format: 'compatible' });
        expect(request.headers.get('X-CSRF-Token')).toBe('token-3');
        return new Response(new Blob(['p12']), { status: 200, headers: { 'Content-Type': 'application/x-pkcs12' } });
      }
      return new Response(JSON.stringify(appliance), { status: 200, headers: { 'Content-Type': 'application/json' } });
    });
    await ask('a long enough secret');
    expect(await screen.findByText(/downloaded/i)).toBeInTheDocument();
    // The link is in the document when clicked, and its URL lives on after the click for the download to start.
    expect(click).toHaveBeenCalledWith('ilm-administrator.p12', 'blob:p12', true, 0);
    expect(URL.revokeObjectURL).not.toHaveBeenCalled();
    await vi.waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:p12'), { timeout: 2000 });
    expect(document.querySelector('a[href="blob:p12"]')).toBeNull();
    expect(screen.queryByLabelText('Certificate password')).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain('a long enough secret');
  });

  it('offers the compatible format by default and sends the modern one when chosen', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/appliance': [200, appliance] });
    renderPage();
    await screen.findByLabelText('Certificate password');
    expect(screen.getByRole('radio', { name: /Compatible \(macOS, Windows, Java\)/ })).toBeChecked();
    expect(screen.getByRole('radio', { name: /Modern \(AES-256\)/ })).not.toBeChecked();
    let sent: unknown;
    mock.mockImplementation(async (request: Request) => {
      if (request.method === 'POST') sent = await request.json();
      return new Response(new Blob(['p12']), { status: 200, headers: { 'Content-Type': 'application/x-pkcs12' } });
    });
    await userEvent.click(screen.getByRole('radio', { name: /Modern \(AES-256\)/ }));
    await ask('a long enough secret');
    expect(await screen.findByText(/downloaded/i)).toBeInTheDocument();
    expect(sent).toEqual({ password: 'a long enough secret', format: 'modern' });
  });

  it('shows a refused password at its field', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      'POST /appliance/api/v1/platform/admin-credential': [400, { code: 'invalid', field: 'password', message: 'Password is too short.' }],
    });
    renderPage();
    await ask('short');
    expect(await screen.findByLabelText('Certificate password')).toHaveAccessibleDescription('Password is too short.');
  });

  it('says the credential was already downloaded', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      'POST /appliance/api/v1/platform/admin-credential': [410, { code: 'downloaded', message: 'x' }],
    });
    renderPage();
    await ask('a long enough secret');
    expect(await screen.findByText(/already downloaded/i)).toBeInTheDocument();
  });

  it('says the renewed credential is not registered in the ILM yet', async () => {
    const message = 'The renewed administrator certificate is not registered in the ILM yet. The dashboard shows why.';
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      'POST /appliance/api/v1/platform/admin-credential': [404, { code: 'not_registered', message }],
    });
    renderPage();
    await ask('a long enough secret');
    expect(await screen.findByRole('alert')).toHaveTextContent(message);
  });

  it('explains other failures', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      'POST /appliance/api/v1/platform/admin-credential': [404, { code: 'not_issued', message: 'x' }],
    });
    renderPage();
    await ask('a long enough secret');
    expect(await screen.findByRole('alert')).toHaveTextContent('not been issued yet');
  });

  it('shows the message the appliance sent for a refused download', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      'POST /appliance/api/v1/platform/admin-credential': [409, { code: 'restore_pending', message: 'The appliance is waiting for a restore.' }],
    });
    renderPage();
    await ask('a long enough secret');
    expect(await screen.findByRole('alert')).toHaveTextContent('waiting for a restore');
  });

  it('says so when the appliance cannot be reached', async () => {
    stubApi({ 'GET /appliance/api/v1/appliance': [500, { code: 'internal', message: 'x' }] });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    expect(screen.getByText(/console now uses the built-in certificate/)).toHaveTextContent('Reload the page and accept the new certificate, or trust the root certificate first');
  });

  it('shows the certificate hint when the download cannot reach the appliance', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/appliance': [200, appliance] });
    renderPage();
    await screen.findByLabelText('Certificate password');
    expect(screen.queryByText(/console now uses the built-in certificate/)).not.toBeInTheDocument();
    mock.mockRejectedValue(new TypeError('certificate changed'));
    await ask('a long enough secret');
    expect(await screen.findByText(/console now uses the built-in certificate/)).toBeInTheDocument();
  });
});
