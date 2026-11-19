import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from './App';
import { api } from './api/client';

type Routes = Record<string, readonly [number, unknown?]>;

function respond(routes: Routes) {
  return vi.fn(async (request: Request) => {
    const [status, body] = routes[new URL(request.url).pathname] ?? [404, { code: 'not_found', message: 'Not found.' }];
    return new Response(body === undefined ? null : JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  });
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

const signedIn: Routes = { '/api/v1/session': [200, { user: 'admin', csrfToken: 'token-1' }] };
const signedOut: Routes = { '/api/v1/session': [401, { code: 'unauthorized', message: 'No session.' }] };

describe('App routing', () => {
  let fetchMock: ReturnType<typeof respond>;

  beforeEach(() => {
    fetchMock = respond({ ...signedOut });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('sends an unauthenticated visitor of / to sign-in', async () => {
    renderAt('/');
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });

  it('sends an unauthenticated visitor of the wizard to sign-in', async () => {
    renderAt('/wizard/');
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });

  it('shows the setup shell banner under /setup/', async () => {
    vi.stubGlobal('fetch', respond({ '/api/v1/setup/state': [200, { step: 'code' }] }));
    renderAt('/setup/');
    expect(await screen.findByRole('banner')).toHaveTextContent('Appliance setup');
  });

  it('shows the user name and a sign-out button in the banner of the status page', async () => {
    vi.stubGlobal('fetch', respond({ ...signedIn, '/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }] }));
    renderAt('/');
    const banner = await screen.findByRole('banner');
    expect(await within(banner).findByText('admin')).toBeInTheDocument();
    expect(within(banner).getByRole('button', { name: 'Sign out' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Sign out' })).toHaveLength(1);
  });

  it('shows the console menu with the current page marked, and the system page', async () => {
    vi.stubGlobal('fetch', respond({ ...signedIn, '/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }] }));
    renderAt('/system');
    expect(await screen.findByRole('heading', { name: 'System' })).toBeInTheDocument();
    expect(within(screen.getByRole('navigation', { name: 'Console' })).getByRole('link', { name: 'System' })).toHaveAttribute('aria-current', 'page');
  });

  it('shows the audit log page', async () => {
    vi.stubGlobal('fetch', respond({ ...signedIn, '/api/v1/audit': [200, { items: [] }] }));
    renderAt('/audit');
    expect(await screen.findByRole('heading', { name: 'Audit log' })).toBeInTheDocument();
    expect(within(screen.getByRole('navigation', { name: 'Console' })).getByRole('link', { name: 'Audit log' })).toHaveAttribute('aria-current', 'page');
  });

  it('shows the sign-in page without a banner and with the theme toggle', async () => {
    renderAt('/signin');
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
    expect(screen.queryByRole('banner')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Theme:/ })).toBeInTheDocument();
  });

  it('shows the setup code page under /setup/ without asking for the console session', async () => {
    fetchMock = respond({ '/api/v1/setup/state': [200, { step: 'code' }] });
    vi.stubGlobal('fetch', fetchMock);
    renderAt('/setup/');
    expect(await screen.findByRole('heading', { name: 'Enter the setup code' })).toBeInTheDocument();
    expect(fetchMock.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual(['/api/v1/setup/state']);
  });

  it('shows the console to a signed-in user', async () => {
    vi.stubGlobal('fetch', respond({ ...signedIn, '/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }] }));
    renderAt('/');
    expect(await screen.findByRole('heading', { name: 'Appliance' })).toBeInTheDocument();
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
  });

  it('shows the wizard to a signed-in user', async () => {
    vi.stubGlobal('fetch', respond({
      ...signedIn,
      '/api/v1/status': [200, { setup: 'part2', conditions: [], versions: {}, rebootRequired: false }],
      '/api/v1/appliance': [200, { node: { hostName: 'node1.example.test' }, dns: [], ntp: [], tlsSource: 'builtin', trustSet: false, registries: [] }],
      '/api/v1/catalogue': [200, { items: [] }],
    }));
    renderAt('/wizard/');
    expect(await screen.findByRole('heading', { name: 'ILM host name' })).toBeInTheDocument();
  });

  it('returns to sign-in when the session ends', async () => {
    vi.stubGlobal('fetch', respond({ ...signedIn, '/api/v1/status': [401, { code: 'unauthorized', message: 'Expired.' }] }));
    renderAt('/');
    await screen.findByRole('heading', { name: 'Appliance' });
    await act(() => api.GET('/api/v1/status'));
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });

  it('shows the done page of the wizard to a signed-in user', async () => {
    vi.stubGlobal('fetch', respond({
      ...signedIn,
      '/api/v1/appliance': [200, { node: { hostName: 'n' }, dns: [], ntp: [], tlsSource: 'builtin', trustSet: false, registries: [] }],
    }));
    renderAt('/wizard/done');
    expect(await screen.findByRole('heading', { name: 'The ILM is ready' })).toBeInTheDocument();
  });

  it('signs in, keeps the session in the context and shows the status page', async () => {
    const done = { setup: 'complete', conditions: [], versions: {}, rebootRequired: false };
    vi.stubGlobal('fetch', respond({ ...signedOut, '/api/v1/status': [200, done] }));
    renderAt('/signin');
    fetchMock = respond({ '/api/v1/session': [200, { csrfToken: 'token-2' }], '/api/v1/status': [200, done] });
    vi.stubGlobal('fetch', fetchMock);
    await userEvent.type(await screen.findByLabelText('User name'), 'admin');
    await userEvent.type(screen.getByLabelText('Password'), 'correct horse battery');
    await userEvent.type(screen.getByLabelText('Authenticator code'), '123456');
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
  });

  it('loads the session once while the user moves between pages', async () => {
    fetchMock = respond({ ...signedIn, '/api/v1/status': [401, { code: 'unauthorized', message: 'Expired.' }] });
    vi.stubGlobal('fetch', fetchMock);
    renderAt('/');
    await screen.findByRole('heading', { name: 'Appliance' });
    await act(() => api.GET('/api/v1/status'));
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
    const sessionReads = fetchMock.mock.calls.filter(([request]) => new URL(request.url).pathname === '/api/v1/session');
    expect(sessionReads).toHaveLength(1);
  });

  it('sends the visitor to sign-in when the session cannot be read', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => {
      throw new TypeError('Failed to fetch');
    }));
    renderAt('/');
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });

  it('sends unknown paths to the console', async () => {
    renderAt('/nowhere');
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });
});
