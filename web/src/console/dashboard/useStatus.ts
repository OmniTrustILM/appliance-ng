import { useEffect, useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { attempt, unreachable } from '../../setup/failure';

export type Status = components['schemas']['Status'];

/** How long a read may take before it counts as failed, so that a connection that hangs does not stop the polling. */
const readTimeoutMs = 10000;

/**
 * Reads the appliance status now and again after each interval; the last good status stays while a read fails.
 * checkedAt is when the last read ended, the time against which the status's expiry dates are judged.
 */
export function useStatus(intervalMs = 5000) {
  const [status, setStatus] = useState<Status>();
  const [error, setError] = useState<string>();
  const [checkedAt, setCheckedAt] = useState(Date.now);

  useEffect(() => {
    let active = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function poll() {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), readTimeoutMs);
      // Marked as a background refresh, the poll does not keep an unattended session from going idle.
      const result = await attempt(() =>
        api.GET('/status', { params: { header: { 'X-Background-Refresh': '1' } }, signal: controller.signal }),
      );
      clearTimeout(timeout);
      if (!active) return;
      setStatus((last) => result?.data ?? last);
      setError(result?.data ? undefined : unreachable);
      setCheckedAt(Date.now());
      timer = setTimeout(poll, intervalMs);
    }
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [intervalMs]);

  return { status, error, checkedAt };
}
