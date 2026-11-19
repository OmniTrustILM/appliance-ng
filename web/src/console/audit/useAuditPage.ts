import { useEffect, useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { attempt, unreachable } from '../../setup/failure';

export type AuditEntry = components['schemas']['AuditEntry'];

/** Entries per page. */
export const pageSize = 25;
/** The appliance returns at most the newest 1000 entries. */
const maxEntries = 1000;

/** How many of the newest entries to read for page (0-based): the page and one more, to know whether a next exists. */
export function limitFor(page: number) {
  return Math.min(maxEntries, (page + 1) * pageSize + 1);
}

/** The entries of page out of the newest entries, and whether a next page exists. */
export function pageOf(items: readonly AuditEntry[], page: number) {
  const end = (page + 1) * pageSize;
  return { entries: items.slice(page * pageSize, end), hasNext: items.length > end };
}

type Loaded = { page: number; entries: AuditEntry[]; hasNext: boolean };

/** Reads one page of the audit log, newest first; the API returns the newest entries up to a limit. */
export function useAuditPage(page: number) {
  const [loaded, setLoaded] = useState<Loaded>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let active = true;
    void attempt(() => api.GET('/api/v1/audit', { params: { query: { limit: limitFor(page) } } })).then((result) => {
      if (!active) return;
      if (result?.data) {
        setLoaded({ page, ...pageOf(result.data.items, page) });
        setError(undefined);
      } else {
        setError(result?.error?.message ?? unreachable);
      }
    });
    return () => {
      active = false;
    };
  }, [page]);

  return { loaded: loaded?.page === page ? loaded : undefined, error };
}
