import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { AuditPage } from './AuditPage';
import { limitFor, pageOf, type AuditEntry } from './useAuditPage';

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

/** Answers like the API: the newest `limit` of `total` entries. */
function stubAudit(total: number) {
  const entries = Array.from({ length: total }, (_, i) => entry(i));
  const limits: number[] = [];
  const { mock } = stubApi({});
  mock.mockImplementation(async (request: Request) => {
    const limit = Number(new URL(request.url).searchParams.get('limit') ?? 100);
    limits.push(limit);
    return new Response(JSON.stringify({ items: entries.slice(0, limit) }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  });
  return limits;
}

function rows() {
  return screen.getAllByRole('row').slice(1);
}

describe('AuditPage', () => {
  it('shows the entries with time, user, action, changed paths and result', async () => {
    stubApi({
      'GET /api/v1/audit': [200, { items: [
        { time: '2026-11-19T12:00:00Z', user: 'admin', action: 'certificate-upload', result: 'failure', paths: ['spec.tls.source', 'spec.tls.secretRef'] },
        { time: '2026-11-19T11:00:00Z', user: '', action: 'sign-in', result: 'denied', paths: [] },
        { time: '2026-11-19T10:00:00Z', user: 'admin', action: 'mail-settings', result: 'success', paths: ['spec.mail'] },
      ] }],
    });
    render(<AuditPage />);
    expect(await screen.findByRole('heading', { name: 'Audit log' })).toBeInTheDocument();
    const [first, second, third] = await screen.findAllByRole('row').then((all) => all.slice(1));
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

  it('pages through the entries, newest first', async () => {
    const limits = stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    expect(rows()).toHaveLength(25);
    expect(limits).toEqual([26]);
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 2');
    expect(rows()).toHaveLength(25);
    expect(within(rows()[0]).getByText(new Date(entry(25).time).toLocaleString())).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByText('Page 3');
    expect(rows()).toHaveLength(10);
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled();
    expect(limits).toEqual([26, 51, 76]);

    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await screen.findByText('Page 2');
    expect(within(rows()[0]).getByText(new Date(entry(25).time).toLocaleString())).toBeInTheDocument();
  });

  it('stops at the newest 1000 entries the appliance returns', () => {
    expect(limitFor(0)).toBe(26);
    expect(limitFor(39)).toBe(1000);
    const page = pageOf(Array.from({ length: 1000 }, (_, i) => entry(i)), 39);
    expect(page.entries).toHaveLength(25);
    expect(page.hasNext).toBe(false);
  });

  it('says so when there are no entries', async () => {
    stubApi({ 'GET /api/v1/audit': [200, { items: [] }] });
    render(<AuditPage />);
    expect(await screen.findByText('Nothing has been recorded yet.')).toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('shows a loading state before the entries arrive', () => {
    stubApi({ 'GET /api/v1/audit': [200, { items: [] }] });
    render(<AuditPage />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading…');
  });

  it('shows an error when the log cannot be read', async () => {
    stubApi({ 'GET /api/v1/audit': [500, { code: 'internal', message: 'The audit log cannot be read.' }] });
    render(<AuditPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('The audit log cannot be read.');
  });

  it('goes back from a page that cannot be read', async () => {
    stubAudit(60);
    render(<AuditPage />);
    await screen.findByText('Page 1');
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    expect(await screen.findByText('Page 1')).toBeInTheDocument();
  });

  it('shows an error when the appliance cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('offline'))));
    render(<AuditPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
