import { act, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { DashboardPage } from './DashboardPage';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const status = {
  setup: 'complete',
  addonStep: 'cert-manager',
  conditions: [
    { type: 'Platform', status: 'True', reason: 'Ready', message: 'ILM is running.' },
    { type: 'Certificate', status: 'False', reason: 'Pending', message: 'Waiting for the issuer.' },
  ],
  versions: { appliance: '1.2.0', talos: 'v1.12.0', kubernetes: 'v1.37.0', ilm: '3.0.0' },
  rebootRequired: true,
};

function renderPage() {
  return render(<MemoryRouter><DashboardPage /></MemoryRouter>);
}

describe('DashboardPage', () => {
  it('shows the setup state, versions, add-on step and conditions', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    renderPage();
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
    expect(screen.getByText('1.2.0')).toBeInTheDocument();
    expect(screen.getByText('v1.12.0')).toBeInTheDocument();
    expect(screen.getByText(/cert-manager/)).toBeInTheDocument();
    expect(screen.getByText('Waiting for the issuer.')).toBeInTheDocument();
    expect(screen.getByText('Pending')).toBeInTheDocument();
    expect(screen.getByText(/restart/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /administrator credential/i })).toHaveAttribute('href', '/wizard/done');
  });

  it('shows the free space of the data volume', async () => {
    const GB = 1024 ** 3;
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, dataVolume: { sizeBytes: 100 * GB, freeBytes: 60 * GB, lowSpace: false } }] });
    renderPage();
    expect(await screen.findByText(/60\.0 GB free of 100\.0 GB/)).toBeInTheDocument();
    expect(screen.queryByText(/running low/i)).not.toBeInTheDocument();
  });

  it('warns when the manager reports the data volume as low on space', async () => {
    const GB = 1024 ** 3;
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, dataVolume: { sizeBytes: 100 * GB, freeBytes: 19 * GB, lowSpace: true } }] });
    renderPage();
    expect(await screen.findByText(/19\.0 GB free of 100\.0 GB/)).toBeInTheDocument();
    expect(screen.getByText(/running low/i)).toBeInTheDocument();
  });

  it('does not judge low space on the rounded free space', async () => {
    const GB = 1024 ** 3;
    // The rounded free space is below a fifth, but the exact one was not.
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, dataVolume: { sizeBytes: 100 * GB + 99, freeBytes: 20 * GB, lowSpace: false } }] });
    renderPage();
    expect(await screen.findByText(/20\.0 GB free/)).toBeInTheDocument();
    expect(screen.queryByText(/running low/i)).not.toBeInTheDocument();
  });

  it('shows no free-space line without the data volume', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    renderPage();
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
    expect(screen.queryByText(/free of/)).not.toBeInTheDocument();
  });

  it('shows the node address and refresh note', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, nodeAddress: '10.0.0.5' }] });
    renderPage();
    expect(await screen.findByText('10.0.0.5')).toBeInTheDocument();
    expect(screen.getByText(/Refreshed every five seconds/)).toBeInTheDocument();
  });

  it('shows the setup badge', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    const { unmount } = renderPage();
    expect(await screen.findByText('Setup complete')).toBeInTheDocument();
    unmount();
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, setup: 'part2' }] });
    renderPage();
    expect(await screen.findByText('Setup in progress')).toBeInTheDocument();
  });

  it('counts ready conditions', async () => {
    const conditions = [
      { type: 'A', status: 'True' },
      { type: 'B', status: 'True' },
      { type: 'C', status: 'False' },
    ];
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, conditions }] });
    renderPage();
    expect(await screen.findByText('2 of 3 ready')).toBeInTheDocument();
  });

  it('shows a loading status before the first status', () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    renderPage();
    expect(screen.getByRole('status')).toHaveTextContent('Loading…');
  });

  it('shows an error alert when the first read fails', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [500, { code: 'internal', message: 'x' }] });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });

  it('marks its reads as background refreshes, which do not keep the session alive', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(mock.mock.calls[0][0].headers.get('X-Background-Refresh')).toBe('1');
  });

  it('points to the wizard while setup is open', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, setup: 'part2', addonStep: undefined, rebootRequired: false }] });
    renderPage();
    expect(await screen.findByText(/not complete/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Continue the setup' })).toHaveAttribute('href', '/wizard/');
  });

  it('refreshes every five seconds and keeps the last state when a refresh fails', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    stubApi({
      'GET /appliance/api/v1/status': () => (++calls === 2 ? [500, { code: 'internal', message: 'x' }] : [200, status]),
    });
    renderPage();
    await screen.findByText('Setup is complete.');
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(calls).toBe(2);
    expect(screen.getByRole('alert')).toHaveTextContent('cannot be reached');
    expect(screen.getByText('Setup is complete.')).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('gives up a read that hangs and polls again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    vi.stubGlobal('fetch', vi.fn((request: Request) => {
      if (!request.url.endsWith('/status')) return Promise.resolve(new Response(null, { status: 404 }));
      if (++calls === 1) {
        return new Promise<Response>((_, reject) => request.signal.addEventListener('abort', () => reject(request.signal.reason)));
      }
      return Promise.resolve(new Response(JSON.stringify(status), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }));
    renderPage();
    await act(() => vi.advanceTimersByTimeAsync(10000));
    expect(screen.getByRole('alert')).toHaveTextContent('cannot be reached');
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(calls).toBe(2);
    expect(await screen.findByText('Setup is complete.')).toBeInTheDocument();
  });

  it('stops polling when it is closed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { mock } = stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    const { unmount } = renderPage();
    await screen.findByText('Setup is complete.');
    unmount();
    await act(() => vi.advanceTimersByTimeAsync(20000));
    const statusReads = mock.mock.calls.filter(([request]) => request.url.endsWith('/status'));
    expect(statusReads).toHaveLength(1);
  });

  it('warns when the certificate expires within 30 days and links to the certificate page', async () => {
    const soon = new Date(Date.now() + 10 * 24 * 60 * 60 * 1000).toISOString();
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, rebootRequired: false, certificate: { notAfter: soon } }] });
    renderPage();
    expect(await screen.findByText(/expires within 30 days/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the certificate page' })).toHaveAttribute('href', '/certificate');
  });

  it('warns about an expired certificate', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, certificate: { notAfter: '2020-01-01T00:00:00Z' } }] });
    renderPage();
    expect(await screen.findByText(/has expired/)).toBeInTheDocument();
  });

  it('does not warn about a certificate valid for longer than 30 days', async () => {
    const later = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, rebootRequired: false, certificate: { notAfter: later } }] });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(screen.queryByText(/expire/)).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Open the certificate page' })).not.toBeInTheDocument();
    expect(screen.getByText(new Date(later).toLocaleDateString())).toBeInTheDocument();
  });

  it('warns 30 days before the recovery kit\'s talosconfig expires and links to the recovery kit page', async () => {
    const soon = new Date(Date.now() + 20 * 24 * 60 * 60 * 1000).toISOString();
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, recoveryKit: { issued: true, confirmed: true, talosconfigNotAfter: soon } }] });
    renderPage();
    expect(await screen.findByText(/talosconfig in the recovery kit expires within 30 days/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the recovery kit page' })).toHaveAttribute('href', '/recovery-kit');
  });

  it('judges the expiry dates again on each refresh while the dashboard stays open', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const day = 24 * 60 * 60 * 1000;
    const certificate = { notAfter: new Date(Date.now() + 10_000).toISOString() };
    const recoveryKit = { issued: true, confirmed: true, talosconfigNotAfter: new Date(Date.now() + 30 * day + 10_000).toISOString() };
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, rebootRequired: false, certificate, recoveryKit }] });
    renderPage();
    expect(await screen.findByText(/certificate expires within 30 days/i)).toBeInTheDocument();
    expect(screen.queryByText(/talosconfig in the recovery kit/i)).not.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(15_000));
    expect(screen.getByText(/certificate has expired/i)).toBeInTheDocument();
    expect(screen.getByText(/talosconfig in the recovery kit expires within 30 days/i)).toBeInTheDocument();
  });

  it('says when the recovery kit\'s talosconfig has expired', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, recoveryKit: { issued: true, confirmed: true, talosconfigNotAfter: '2020-01-01T00:00:00Z' } }] });
    renderPage();
    expect(await screen.findByText(/talosconfig in the recovery kit has expired/i)).toBeInTheDocument();
  });

  it('does not warn about a recovery kit valid for longer than 30 days', async () => {
    const later = new Date(Date.now() + 200 * 24 * 60 * 60 * 1000).toISOString();
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, recoveryKit: { issued: true, confirmed: true, talosconfigNotAfter: later } }] });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(screen.queryByText(/recovery kit/i)).not.toBeInTheDocument();
  });

  it('shows no recovery kit warning while the status leaves the kit out', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(screen.queryByText(/recovery kit/i)).not.toBeInTheDocument();
  });

  it('warns about a recovery kit that is not confirmed once setup is complete', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, recoveryKit: { issued: true, confirmed: false } }] });
    const first = renderPage();
    expect(await screen.findByText(/recovery kit is not confirmed/i)).toBeInTheDocument();
    first.unmount();
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, setup: 'part2' }] });
    renderPage();
    await screen.findByText('Setup is not complete.');
    expect(screen.queryByText(/recovery kit/i)).not.toBeInTheDocument();
  });

  it('links the restart warning to the system page', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, status] });
    renderPage();
    expect(await screen.findByText(/needs to restart/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the system page' })).toHaveAttribute('href', '/system');
  });

  it('shows no restart warning when none is needed', async () => {
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, rebootRequired: false }] });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(screen.queryByRole('link', { name: 'Open the system page' })).not.toBeInTheDocument();
  });

  it('names every version', async () => {
    const versions = { appliance: '0.2.0', talos: 'v1.14.5', kubernetes: 'v1.37.1', operator: 'v1.1.0', ilm: '2.20.0' };
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, versions }] });
    renderPage();
    const card = (await screen.findByRole('heading', { name: 'Versions' })).closest('div') as HTMLElement;
    for (const [label, version] of [['Appliance', '0.2.0'], ['Talos', 'v1.14.5'], ['Kubernetes', 'v1.37.1'], ['ILM operator', 'v1.1.0'], ['ILM', '2.20.0']]) {
      expect(within(card).getByText(label).nextElementSibling).toHaveTextContent(version);
    }
  });

  it('groups the conditions by area', async () => {
    const conditions = [
      { type: 'NodeConfigured', status: 'True', reason: 'Applied' },
      { type: 'ConnectorsReady', status: 'False', reason: 'Progressing', message: 'waiting for these connectors: ejbca-ng-connector' },
      { type: 'PlatformReady', status: 'True', reason: 'Ready' },
      { type: 'CertificateReady', status: 'True', reason: 'Ready' },
      { type: 'ILMSettingsApplied', status: 'True', reason: 'Applied' },
      { type: 'CBOMRepositoryApplied', status: 'True', reason: 'Applied' },
    ];
    stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, conditions }] });
    renderPage();
    const platform = (await screen.findByRole('heading', { name: 'Platform', level: 3 })).parentElement as HTMLElement;
    expect(within(platform).getByText('ConnectorsReady')).toBeInTheDocument();
    expect(within(platform).getByText('PlatformReady')).toBeInTheDocument();
    expect(within(platform).getByText('ILMSettingsApplied')).toBeInTheDocument();
    expect(within(platform).getByText('CBOMRepositoryApplied')).toBeInTheDocument();
    expect(within(platform).queryByText('NodeConfigured')).not.toBeInTheDocument();
    const node = screen.getByRole('heading', { name: 'Node', level: 3 }).parentElement as HTMLElement;
    expect(within(node).getByText('NodeConfigured')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Certificate', level: 3 })).toBeInTheDocument();
    expect(screen.getByText('5 of 6 ready')).toBeInTheDocument();
  });

  describe('backups', () => {
    const day = 24 * 60 * 60 * 1000;
    const ago = (days: number) => new Date(Date.now() - days * day).toISOString();
    const backup = (over: object = {}) => ({
      running: false,
      last: { name: 'a.ilmbackup', time: ago(1), sizeBytes: 10, database: 'included' },
      targets: [
        { name: 'offsite', warning: false, lastSuccess: ago(1) },
        { name: 'nas', warning: true, lastError: 'unreachable' },
      ],
      lastRestoreTest: ago(10),
      ...over,
    });

    it('shows the last backup and each target\'s last success and error', async () => {
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, backup: backup({ running: true }) }] });
      renderPage();
      expect(await screen.findByText(/Last backup/)).toBeInTheDocument();
      expect(screen.getByText(/A backup is running/)).toBeInTheDocument();
      expect(screen.getByText(/last success never/)).toBeInTheDocument();
      expect(screen.getByText('The target cannot be reached from the appliance or through its proxy.')).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Manage the backups' })).toHaveAttribute('href', '/backups');
      expect(screen.getByText(/backup target nas has had no successful copy for more than twice its interval/i)).toBeInTheDocument();
      expect(screen.queryByText(/restore test/i)).not.toBeInTheDocument();
    });

    it('marks a last backup without the database and says who backs it up', async () => {
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, backup: backup({ last: { name: 'a', time: ago(1), sizeBytes: 1, database: 'notIncluded' } }) }] });
      renderPage();
      expect(await screen.findByText(/external database's owner must back it up/)).toBeInTheDocument();
    });

    it('says when no backup was taken yet', async () => {
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, backup: backup({ last: undefined, targets: [] }) }] });
      renderPage();
      expect(await screen.findByText('No backup was taken yet.')).toBeInTheDocument();
    });

    it('reminds of a restore test when none is recorded or the last one is older than 90 days', async () => {
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, backup: backup({ lastRestoreTest: undefined }) }] });
      const first = renderPage();
      expect(await screen.findByText(/no restore test is recorded/i)).toBeInTheDocument();
      first.unmount();
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, backup: backup({ lastRestoreTest: ago(91) }) }] });
      const second = renderPage();
      expect(await screen.findByText(/last restore test is more than 90 days old/i)).toBeInTheDocument();
      second.unmount();
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, setup: 'part2', backup: backup({ lastRestoreTest: undefined }) }] });
      renderPage();
      expect(await screen.findByText(/setup is not complete/i)).toBeInTheDocument();
      expect(screen.queryByText(/restore test/i)).not.toBeInTheDocument();
    });

    it('judges the restore test again on each refresh', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, backup: backup({ lastRestoreTest: new Date(Date.now() - 90 * day + 10_000).toISOString() }) }] });
      renderPage();
      expect(await screen.findByText(/Last backup/)).toBeInTheDocument();
      expect(screen.queryByText(/restore test/i)).not.toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(15_000));
      expect(screen.getByText(/last restore test is more than 90 days old/i)).toBeInTheDocument();
    });

    it('warns when backups are not healthy and groups the condition under backups', async () => {
      const conditions = [{ type: 'BackupHealthy', status: 'False', reason: 'NoKit', message: 'no backup until the recovery kit is confirmed' }];
      stubApi({ 'GET /appliance/api/v1/status': [200, { ...status, conditions, backup: backup() }] });
      renderPage();
      expect(await screen.findByText(/backups are not healthy: no backup until the recovery kit is confirmed/i)).toBeInTheDocument();
      expect(screen.getByRole('heading', { name: 'Backups', level: 3 })).toBeInTheDocument();
    });
  });

  it('notices an available update that can be installed, and not a blocked one', async () => {
    const bundle = { kind: 'platform', version: '2.1.0', arch: 'amd64', sizeBytes: 1, notes: '/n' };
    stubApi({
      'GET /appliance/api/v1/status': [200, status],
      'GET /appliance/api/v1/updates': [200, { available: [bundle, { ...bundle, kind: 'system', blocked: 'sameVersion' }], loaded: [] }],
    });
    renderPage();
    expect(await screen.findByText(/newer platform update is available/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /updates page/i })).toHaveAttribute('href', '/updates');
  });

  it('shows no update notice when nothing installable is available or the updates cannot be read', async () => {
    stubApi({
      'GET /appliance/api/v1/status': [200, status],
      'GET /appliance/api/v1/updates': [200, { available: [{ kind: 'system', version: '1.0.0', arch: 'amd64', sizeBytes: 1, notes: '/n', blocked: 'olderThanRunning' }], loaded: [] }],
    });
    renderPage();
    await screen.findByText('Setup is complete.');
    expect(screen.queryByText(/update is available/i)).not.toBeInTheDocument();
  });
});
