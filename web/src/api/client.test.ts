import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, createApiClient, requestTimeoutMs, setCsrfToken, setUnauthorizedHandler, timeoutSignal } from './client';

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

  const client = createApiClient('http://appliance.test/appliance/api/v1');
  const sent = () => fetchMock.mock.calls[0][0];

  it('adds the CSRF token to requests that change state', async () => {
    setCsrfToken('token-1');
    await client.DELETE('/session');
    expect(sent().headers.get('X-CSRF-Token')).toBe('token-1');
  });

  it('does not add the CSRF token to GET requests', async () => {
    setCsrfToken('token-1');
    await client.GET('/status');
    expect(sent().headers.has('X-CSRF-Token')).toBe(false);
  });

  it('sends no CSRF header without a token', async () => {
    await client.POST('/setup/apply');
    expect(sent().headers.has('X-CSRF-Token')).toBe(false);
  });

  it("sends the console's requests under /appliance/api/v1", async () => {
    await api.GET('/status');
    expect(new URL(sent().url).pathname).toBe('/appliance/api/v1/status');
  });

  it('sends cookies with every request', async () => {
    await client.GET('/status');
    expect(sent().credentials).toBe('same-origin');
  });

  it('routes a 401 to sign-in', async () => {
    vi.stubGlobal('fetch', reply(401, { code: 'unauthorized', message: 'Sign in again.' }));
    const { error } = await client.GET('/status');
    expect(unauthorized).toHaveBeenCalledOnce();
    expect(error?.code).toBe('unauthorized');
  });

  it.each([
    ['the sign-in calls', '/session'],
    ['wizard part 1', '/setup/dhcp'],
  ] as const)('leaves a 401 from %s to the caller', async (_name, path) => {
    vi.stubGlobal('fetch', reply(401, { code: 'unauthorized', message: 'No session.' }));
    await client.GET(path);
    expect(unauthorized).not.toHaveBeenCalled();
  });
});

describe('timeoutSignal', () => {
  afterEach(() => vi.useRealTimers());

  it('aborts after the read limit by default, or after the given time', () => {
    vi.useFakeTimers();
    const read = timeoutSignal();
    const slow = timeoutSignal(2 * requestTimeoutMs);
    vi.advanceTimersByTime(requestTimeoutMs - 1);
    expect(read.aborted).toBe(false);
    vi.advanceTimersByTime(1);
    expect(read.aborted).toBe(true);
    expect(read.reason).toBeInstanceOf(DOMException);
    expect(slow.aborted).toBe(false);
    vi.advanceTimersByTime(requestTimeoutMs);
    expect(slow.aborted).toBe(true);
  });
});
