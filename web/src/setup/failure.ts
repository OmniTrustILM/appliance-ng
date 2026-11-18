/** Runs a request; undefined means the appliance could not be reached. */
export async function attempt<R>(call: () => Promise<R>): Promise<R | undefined> {
  try {
    return await call();
  } catch {
    return undefined;
  }
}

export const unreachable = 'The appliance cannot be reached. Check the connection and try again.';

/** The message for a response the page has no specific text for. */
export function failureMessage(status: number): string {
  if (status === 403) return 'The request was refused. Reload the page and try again.';
  if (status === 409) return 'The setup is not at this step. Reload the page to continue where it stands.';
  return 'The appliance could not process the request. Reload the page and try again.';
}
