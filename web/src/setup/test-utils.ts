import { vi } from 'vitest';

type Reply = readonly [number, unknown?, Record<string, string>?];
/** An answer that never comes: the request ends only when it is aborted. */
export const hang = 'hang';
type Handler = Reply | typeof hang | ((body: unknown) => Reply | typeof hang | Promise<Reply>);

async function readBody(request: Request): Promise<unknown> {
  if (request.headers.get('Content-Type')?.startsWith('multipart/form-data')) {
    return Object.fromEntries((await request.formData()).entries());
  }
  if (request.headers.get('Content-Type') === 'application/octet-stream') return request.text();
  const text = await request.text();
  return text ? JSON.parse(text) : undefined;
}

/**
 * Stubs fetch with answers keyed by "METHOD /path"; unknown routes answer 404.
 * `sent` collects the bodies of the requests per key: parsed JSON, the fields of a multipart body, or the text of
 * an octet stream.
 * A string payload is answered as PEM text; a third element adds response headers. `hang` never answers; a handler
 * may answer later with a promise.
 */
export function stubApi(routes: Record<string, Handler>) {
  const sent: Record<string, unknown[]> = {};
  const mock = vi.fn(async (request: Request) => {
    const key = `${request.method} ${new URL(request.url).pathname}`;
    const body = request.method === 'GET' ? undefined : await readBody(request);
    sent[key] ??= [];
    sent[key].push(body);
    const handler = routes[key];
    const reply = typeof handler === 'function' ? await handler(body) : (handler ?? [404, { code: 'not_found', message: 'Not found.' }]);
    if (reply === hang) {
      return new Promise<Response>((_, reject) => request.signal.addEventListener('abort', () => reject(request.signal.reason)));
    }
    const [status, payload, headers] = reply;
    if (typeof payload === 'string') {
      return new Response(payload, { status, headers: { 'Content-Type': 'application/x-pem-file', ...headers } });
    }
    return new Response(payload === undefined ? null : JSON.stringify(payload), {
      status,
      headers: { 'Content-Type': 'application/json', ...headers },
    });
  });
  vi.stubGlobal('fetch', mock);
  return { mock, sent };
}
