import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createApiClient, setCsrfToken, setUnauthorizedHandler } from './client';

function reply(status: number, body: unknown = {}) {
  return vi.fn<(request: Request) => Promise<Response>>(async () => new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  }));
}

describe('api client', () => {
  let fetchMock: ReturnType<typeof reply>;
  let unauthorized: () => void;

  beforeEach(() => {
    fetchMock = reply(204);
    vi.stubGlobal('fetch', fetchMock);
    unauthorized = vi.fn();
    setUnauthorizedHandler(unauthorized);
    setCsrfToken(undefined);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const client = createApiClient('http://appliance.test');
  const sent = () => fetchMock.mock.calls[0][0];

  it('adds the CSRF token to requests that change state', async () => {
    setCsrfToken('token-1');
    await client.DELETE('/api/v1/session');
    expect(sent().headers.get('X-CSRF-Token')).toBe('token-1');
  });

  it('does not add the CSRF token to GET requests', async () => {
    setCsrfToken('token-1');
    await client.GET('/api/v1/status');
    expect(sent().headers.has('X-CSRF-Token')).toBe(false);
  });

  it('sends no CSRF header without a token', async () => {
    await client.POST('/api/v1/setup/apply');
    expect(sent().headers.has('X-CSRF-Token')).toBe(false);
  });

  it('sends cookies with every request', async () => {
    await client.GET('/api/v1/status');
    expect(sent().credentials).toBe('same-origin');
  });

  it('routes a 401 to sign-in', async () => {
    vi.stubGlobal('fetch', reply(401, { code: 'unauthorized', message: 'Sign in again.' }));
    const { error } = await client.GET('/api/v1/status');
    expect(unauthorized).toHaveBeenCalledOnce();
    expect(error?.code).toBe('unauthorized');
  });

  it.each([
    ['the sign-in calls', '/api/v1/session'],
    ['wizard part 1', '/api/v1/setup/dhcp'],
  ] as const)('leaves a 401 from %s to the caller', async (_name, path) => {
    vi.stubGlobal('fetch', reply(401, { code: 'unauthorized', message: 'No session.' }));
    await client.GET(path);
    expect(unauthorized).not.toHaveBeenCalled();
  });
});
