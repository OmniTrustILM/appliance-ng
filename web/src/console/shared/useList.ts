import { useLoaded } from './useLoaded';

type Loaded<T> = { data?: { items: T[] }; error?: { message: string } };

/** Loads a list when the page opens and again on `reload`; `load` must be a stable function. */
export function useList<T>(load: () => Promise<Loaded<T>>) {
  const { data, error, reload } = useLoaded(load);
  return { items: data?.items, error, reload };
}
