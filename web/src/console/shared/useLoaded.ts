import { useCallback, useEffect, useState } from 'react';
import { attempt, unreachable } from '../../setup/failure';

type Loaded<T> = { data?: T; error?: { message: string } };

/** Loads one resource when the page opens and again on `reload`; `load` must be a stable function. */
export function useLoaded<T>(load: () => Promise<Loaded<T>>) {
  const [reads, setReads] = useState(0);
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let active = true;
    void attempt(load).then((result) => {
      if (!active) return;
      if (result?.data) {
        setData(result.data);
        setError(undefined);
      } else {
        setError(result?.error?.message ?? unreachable);
      }
    });
    return () => {
      active = false;
    };
  }, [load, reads]);

  const reload = useCallback(() => setReads((count) => count + 1), []);
  return { data, error, reload };
}
