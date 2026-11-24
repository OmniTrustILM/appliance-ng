import { useEffect, useState } from 'react';
import { api } from '../../api/client';
import type { components } from '../../api/schema';
import { attempt, unreachable } from '../../setup/failure';

export type AuditEntry = components['schemas']['AuditEntry'];

export const pageSize = 25;

/** A page (0-based) and the cursors of the pages up to it; the first page has none. */
type Request = { page: number; cursors: (string | undefined)[] };
/** A page on screen with its entries and the cursors of the pages up to the one after it. */
type Shown = Request & { entries: AuditEntry[] };

/**
 * Reads the audit log a page at a time with the server's cursors, so new entries do not shift the pages that follow
 * the first. A page that cannot be read leaves the shown one in place.
 */
export function useAuditPages() {
  const [request, setRequest] = useState<Request>({ page: 0, cursors: [undefined] });
  const [shown, setShown] = useState<Shown>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let active = true;
    const { page, cursors } = request;
    const before = cursors[page];
    const query = before === undefined ? { limit: pageSize } : { limit: pageSize, before };
    void attempt(() => api.GET('/audit', { params: { query } })).then((result) => {
      if (!active) return;
      setLoading(false);
      if (!result?.data) return setError(result?.error?.message ?? unreachable);
      const known = cursors.slice(0, page + 1);
      const { items, next } = result.data;
      setShown({ page, entries: items, cursors: next === undefined ? known : [...known, next] });
      setError(undefined);
    });
    return () => {
      active = false;
    };
  }, [request]);

  function go(page: number, cursors: (string | undefined)[]) {
    setLoading(true);
    setRequest({ page, cursors });
  }

  return {
    shown,
    error,
    loading,
    hasNext: !!shown && shown.cursors.length > shown.page + 1,
    next: () => shown && go(shown.page + 1, shown.cursors),
    previous: () => shown && go(shown.page - 1, shown.cursors),
    refresh: () => go(0, [undefined]),
  };
}
