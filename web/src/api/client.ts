import createClient, { type Middleware } from 'openapi-fetch';
import { apiBase, basePath } from '../base';
import type { paths } from './schema';

const safeMethods = new Set(['GET', 'HEAD', 'OPTIONS']);

let csrfToken: string | undefined;
let onUnauthorized = () => window.location.assign(`${basePath}/signin`);

/** Sets the session's CSRF token, which the client sends on every request that changes state. */
export function setCsrfToken(token: string | undefined) {
  csrfToken = token;
}

/** Sets what happens when a request of the signed-in console gets a 401. */
export function setUnauthorizedHandler(handler: () => void) {
  onUnauthorized = handler;
}

// Wizard part 1 has its own session, and the sign-in calls answer 401 to report a failed or missing sign-in.
function leavesUnauthorizedToCaller(path: string) {
  return path.startsWith(`${apiBase}/setup/`) || path === `${apiBase}/session`;
}

const session: Middleware = {
  onRequest({ request }) {
    if (csrfToken && !safeMethods.has(request.method)) {
      request.headers.set('X-CSRF-Token', csrfToken);
    }
    return request;
  },
  onResponse({ request, response }) {
    if (response.status === 401 && !leavesUnauthorizedToCaller(new URL(request.url).pathname)) {
      onUnauthorized();
    }
    return response;
  },
};

/** Creates a client of the appliance API at baseUrl, which ends in the API's base path. */
export function createApiClient(baseUrl: string) {
  const client = createClient<paths>({
    baseUrl,
    credentials: 'same-origin',
    // Looked up on each call, so the fetch in use is the current global one.
    fetch: (request) => globalThis.fetch(request),
  });
  client.use(session);
  return client;
}

/** The client of the appliance that served the console. */
export const api = createApiClient(window.location.origin + apiBase);
