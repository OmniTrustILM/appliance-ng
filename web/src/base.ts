/** The path the console and its API are served under, apart from ILM on a shared host name. */
export const basePath = '/appliance';

/** The router's basename; the trailing slash keeps the console's root at /appliance/. */
export const routerBasename = `${basePath}/`;

/** The path of the appliance API, the server base of api/openapi.yaml. */
export const apiBase = `${basePath}/api/v1`;
