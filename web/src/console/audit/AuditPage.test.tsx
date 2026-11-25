import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { AuditPage } from './AuditPage';
import type { AuditEntry } from './useAuditPages';

afterEach(() => vi.unstubAllGlobals());

function entry(i: number): AuditEntry {
  return {
    time: new Date(Date.UTC(2026, 10, 19, 12, 0, 0) - i * 60000).toISOString(),
    user: i % 2 ? 'admin' : '',
    action: 'node-settings',
    result: 'success',
    paths: i % 2 ? ['spec.dns.servers', 'spec.ntp.servers'] : [],
  };
}

/**
 * Answers like the API: the entries newest first, `limit` at a time, before the entry the cursor names. The cursor is
 * the number of the last entry of the page; entries added with `add` are newer and do not move it. Each query is kept.
 */
function stubAudit(total: number) {
  let newest = total;
  const queries: string[] = [];
  const { mock } = stubApi({});
  mock.mockImplementation(async (request: Request) => {
    const params = new URL(request.url).searchParams;
    queries.push(params.toString());
    const limit = Number(params.get('limit') ?? 100);
    const before = params.get('before');
    const from = before === null ? newest - 1 : Number(before) - 1;
    const numbers = Array.from({ length: Math.max(Math.min(limit, from + 1), 0) }, (_, i) => from - i);
    const items = numbers.map((n) => entry(total - 1 - n));
    const last = numbers.at(-1);
    const next = last !== undefined && last > 0 ? String(last) : undefined;
    return new Response(JSON.stringify({ items, next }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  });
  return { queries, add: (n: number) => { newest += n; } };
}

function rows() {
  return screen.getAllByRole('row').slice(1);
}

describe('AuditPage', () => {
  it('shows the entries with time, user, action, changed paths and result', async () => {
    stubApi({
      'GET /appliance/api/v1/audit': [200, { items: [
        { time: '2026-11-19T12:00:00Z', user: 'admin', action: 'certificate-upload', result: 'failure', paths: ['spec.tls.source', 'spec.tls.secretRef'] },
        { time: '2026-11-19T11:00:00Z', user: '', action: 'sign-in', result: 'denied', paths: [] },
        { time: '2026-11-19T10:00:00Z', user: 'admin', action: 'mail-settings', result: 'success', paths: ['spec.mail'] },
        { time: '2026-11-19T09:00:00Z', user: 'admin', action: 'kubeconfig-revoke', result: 'success', paths: [], details: { role: 'view', name: 'kc-abc12' } },
      ] }],
    });
    render(<AuditPage />);
    expect(await screen.findByRole('heading', { name: 'Audit log' })).toBeInTheDocument();
    const [first, second, third, fourth] = await screen.findAllByRole('row').then((all) => all.slice(1));
    expect(within(fourth).getByText('role: view, name: kc-abc12')).toBeInTheDocument();
    expect(within(first).getByText('admin')).toBeInTheDocument();
    expect(within(first).getByText('certificate-upload')).toBeInTheDocument();
    expect(within(first).getByText('spec.tls.source, spec.tls.secretRef')).toBeInTheDocument();
    expect(within(first).getByText('Failed')).toBeInTheDocument();
    expect(within(first).getByText(new Date('2026-11-19T12:00:00Z').toLocaleString())).toBeInTheDocument();
    expect(within(second).getByText('Denied')).toBeInTheDocument();
    expect(within(second).getAllByText('—')).toHaveLength(2);
    expect(within(third).getByText('Success')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Next' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Previous' })).toBeDisabled();
  });

  it('shows why a change failed or was denied under its details', async () => {
    stubApi({
      'GET /appliance/api/v1/audit': [200, { items: [
        { time: '2026-11-19T12:00:00Z', user: 'admin', action: 'backup-target-set', result: 'failure', paths: [],
          details: { name: 'a', status: '409', code: 'busy', message: 'Try again later.' } },
        { time: '2026-11-19T11:00:00Z', user: 'admin', action: 'wizard-platform', result: 'denied', paths: [],
          details: { status: '403', code: 'csrf', message: 'The request lacks a valid CSRF token.' } },
        { time: '2026-11-19T10:00:00Z', user: 'admin', action: 'admin-credential', result: 'failure', paths: [],
          details: { status: '500', code: 'internal', message: 'The request failed; the manager log has the details.', requestId: '0123456789abcdef' } },
        { time: '2026-11-19T09:00:00Z', user: 'admin', action: 'kubeconfig-revoke', result: 'success', paths: [], details: { message: 'not a reason' } },
      ] }],
    });
    render(<AuditPage />);
    const [first, second, third, fourth] = await screen.findAllByRole('row').then((all) => all.slice(1));
    expect(within(first).getByText('name: a, status: 409, code: busy')).toBeInTheDocument();
    expect(within(first).getByText('Try again later.')).toBeInTheDocument();
    expect(within(second).getByText('The request lacks a valid CSRF token.')).toBeInTheDocument();
    expect(within(third).getByText('Internal error, request 0123456789abcdef')).toBeInTheDocument();
    expect(within(third).queryByText(/manager log/)).not.toBeInTheDocument();
    expect(within(fourth).getByText('message: not a reason')).toBeInTheDocument();
  });

  it('pages by cursor from the server', async () => {
    const { queries } = stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    expect(rows()).toHaveLength(25);
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 2');
    expect(within(rows()[0]).getByText(new Date(entry(25).time).toLocaleString())).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 3');
    expect(rows()).toHaveLength(10);
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled();

    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await screen.findByText('Page 2');
    expect(within(rows()[0]).getByText(new Date(entry(25).time).toLocaleString())).toBeInTheDocument();
    expect(queries).toEqual(['limit=25', 'limit=25&before=35', 'limit=25&before=10', 'limit=25&before=35']);
  });

  it('keeps the following pages in place when entries are added while paging', async () => {
    const { add } = stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 2');
    const second = rows().map((row) => row.textContent);
    add(5);
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 3');
    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await screen.findByText('Page 2');
    expect(rows().map((row) => row.textContent)).toEqual(second);
  });

  it('starts again at the newest entries on refresh', async () => {
    const { queries } = stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 2');
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await screen.findByText('Page 1');
    expect(queries.at(-1)).toBe('limit=25');
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();
  });

  it('stays on the page and reports a next page that cannot be read', async () => {
    stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    expect(screen.getByText('Page 1')).toBeInTheDocument();
    expect(rows()).toHaveLength(25);
  });

  it('says so when a later page comes back empty', async () => {
    const { mock } = stubApi({});
    mock.mockImplementation(async (request: Request) => {
      const later = new URL(request.url).searchParams.has('before');
      const page = later ? { items: [] } : { items: Array.from({ length: 25 }, (_, i) => entry(i)), next: 'gone' };
      return new Response(JSON.stringify(page), { status: 200, headers: { 'Content-Type': 'application/json' } });
    });
    render(<AuditPage />);
    await screen.findByText('Page 1');
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('No older entries are left.')).toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Previous' })).toBeEnabled();
  });

  it('says so when there are no entries', async () => {
    stubApi({ 'GET /appliance/api/v1/audit': [200, { items: [] }] });
    render(<AuditPage />);
    expect(await screen.findByText('Nothing has been recorded yet.')).toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('shows a loading state before the entries arrive', () => {
    stubApi({ 'GET /appliance/api/v1/audit': [200, { items: [] }] });
    render(<AuditPage />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading…');
  });

  it('shows an error when the log cannot be read', async () => {
    stubApi({ 'GET /appliance/api/v1/audit': [500, { code: 'internal', message: 'The audit log cannot be read.' }] });
    render(<AuditPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('The audit log cannot be read.');
  });

  it('keeps the page and reports a refresh that fails', async () => {
    stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    expect(rows()).toHaveLength(25);
  });

  it('shows an error when the appliance cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('offline'))));
    render(<AuditPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
