import { useCallback, useEffect, useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { attempt, unreachable } from '../../setup/failure';

export type AuditEntry = components['schemas']['AuditEntry'];

/** Entries per page. */
export const pageSize = 25;
/** The most the appliance returns in one read: the newest 1000 entries. */
const maxEntries = 1000;

/** The entries of page (0-based) out of the snapshot, and whether a next page exists. */
export function pageOf(items: readonly AuditEntry[], page: number) {
  const end = (page + 1) * pageSize;
  return { entries: items.slice(page * pageSize, end), hasNext: items.length > end };
}

/** Reads one snapshot of the newest audit entries when opened and again on refresh, so paging never shifts. */
export function useAuditSnapshot() {
  const [items, setItems] = useState<AuditEntry[]>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);
  const [round, setRound] = useState(0);

  useEffect(() => {
    let active = true;
    void attempt(() => api.GET('/api/v1/audit', { params: { query: { limit: maxEntries } } })).then((result) => {
      if (!active) return;
      if (result?.data) {
        setItems(result.data.items);
        setError(undefined);
      } else {
        setError(result?.error?.message ?? unreachable);
      }
      setLoading(false);
    });
    return () => {
      active = false;
    };
  }, [round]);

  const refresh = useCallback(() => {
    setLoading(true);
    setRound((n) => n + 1);
  }, []);
  return { items, error, loading, refresh };
}
