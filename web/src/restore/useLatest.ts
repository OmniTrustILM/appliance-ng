import { useCallback, useEffect, useRef } from 'react';

/**
 * Tells which request of a component is the latest: `begin` marks a new one and returns whether it is still the
 * latest. A request is superseded by the next `begin`, and by the component leaving the page.
 */
export function useLatest() {
  const latest = useRef(0);
  useEffect(() => () => {
    latest.current++;
  }, []);
  return useCallback(() => {
    const mine = ++latest.current;
    return () => latest.current === mine;
  }, []);
}
