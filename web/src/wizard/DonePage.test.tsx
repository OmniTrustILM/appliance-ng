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

describe('DonePage', () => {
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
        expect(await request.json()).toEqual({ password: 'a long enough secret' });
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

  it('explains other failures', async () => {
    stubApi({
      'GET /appliance/api/v1/appliance': [200, appliance],
      'POST /appliance/api/v1/platform/admin-credential': [404, { code: 'not_issued', message: 'x' }],
    });
    renderPage();
    await ask('a long enough secret');
    expect(await screen.findByRole('alert')).toHaveTextContent('not been issued yet');
  });

  it('says so when the appliance cannot be reached', async () => {
    stubApi({ 'GET /appliance/api/v1/appliance': [500, { code: 'internal', message: 'x' }] });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
