import { vi } from 'vitest';

type Reply = readonly [number, unknown?];
type Handler = Reply | ((body: unknown) => Reply);

/**
 * Stubs fetch with answers keyed by "METHOD /path"; unknown routes answer 404.
 * `sent` collects the JSON bodies of the requests per key.
 */
export function stubApi(routes: Record<string, Handler>) {
  const sent: Record<string, unknown[]> = {};
  const mock = vi.fn(async (request: Request) => {
    const key = `${request.method} ${new URL(request.url).pathname}`;
    const text = request.method === 'GET' ? '' : await request.text();
    const body = text ? JSON.parse(text) : undefined;
    (sent[key] ??= []).push(body);
    const handler = routes[key];
    const [status, payload] = typeof handler === 'function'
      ? handler(body)
      : (handler ?? [404, { code: 'not_found', message: 'Not found.' }]);
    return new Response(payload === undefined ? null : JSON.stringify(payload), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  });
  vi.stubGlobal('fetch', mock);
  return { mock, sent };
}
