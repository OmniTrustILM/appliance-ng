import { vi } from 'vitest';

type Reply = readonly [number, unknown?];
type Handler = Reply | ((body: unknown) => Reply);

async function readBody(request: Request): Promise<unknown> {
  if (request.headers.get('Content-Type')?.startsWith('multipart/form-data')) {
    return Object.fromEntries((await request.formData()).entries());
  }
  const text = await request.text();
  return text ? JSON.parse(text) : undefined;
}

/**
 * Stubs fetch with answers keyed by "METHOD /path"; unknown routes answer 404.
 * `sent` collects the bodies of the requests per key: parsed JSON, or the fields of a multipart body.
 * A string payload is answered as PEM text.
 */
export function stubApi(routes: Record<string, Handler>) {
  const sent: Record<string, unknown[]> = {};
  const mock = vi.fn(async (request: Request) => {
    const key = `${request.method} ${new URL(request.url).pathname}`;
    const body = request.method === 'GET' ? undefined : await readBody(request);
    sent[key] ??= [];
    sent[key].push(body);
    const handler = routes[key];
    const [status, payload] = typeof handler === 'function'
      ? handler(body)
      : (handler ?? [404, { code: 'not_found', message: 'Not found.' }]);
    if (typeof payload === 'string') {
      return new Response(payload, { status, headers: { 'Content-Type': 'application/x-pem-file' } });
    }
    return new Response(payload === undefined ? null : JSON.stringify(payload), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  });
  vi.stubGlobal('fetch', mock);
  return { mock, sent };
}
