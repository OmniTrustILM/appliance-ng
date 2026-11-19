import { useState } from 'react';
import { attempt, unreachable } from '../../setup/failure';

type Sent = { response: Response; error?: { message: string } };

/** The confirm-then-delete flow of a list: `ask` opens the confirmation for an item, `confirm` deletes it. */
export function useRemoval<T>(remove: (item: T) => Promise<Sent>, onRemoved: (item: T) => void) {
  const [asking, setAsking] = useState<T>();
  const [error, setError] = useState<string>();

  async function confirm() {
    if (asking === undefined) return;
    const item = asking;
    setAsking(undefined);
    setError(undefined);
    const result = await attempt(() => remove(item));
    if (!result) return setError(unreachable);
    if (!result.response.ok) return setError(result.error?.message ?? 'The appliance could not remove it.');
    onRemoved(item);
  }

  return { asking, error, ask: setAsking, cancel: () => setAsking(undefined), confirm: () => void confirm() };
}
