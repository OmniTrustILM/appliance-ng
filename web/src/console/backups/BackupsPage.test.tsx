import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { BackupsPage, runningRefreshMs } from './BackupsPage';

afterEach(() => vi.unstubAllGlobals());

const BACKUPS = 'GET /appliance/api/v1/backups';
const noSecrets = { accessKey: false, secretKey: false, ca: false, password: false, privateKey: false };
const fingerprint = 'SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG';

const s3Target = {
  name: 'offsite', type: 's3', schedule: '0 2 * * *', timeZone: 'Europe/Prague', retention: 14,
  s3: { endpoint: 'https://s3.example.test', bucket: 'ilm', prefix: 'appliance' },
  secretsSet: { ...noSecrets, accessKey: true, secretKey: true },
  health: { name: 'offsite', warning: false, lastSuccess: '2026-11-20T02:01:00Z' },
};
const sftpTarget = {
  name: 'sftp', type: 'sftp', schedule: '30 1 * * *', timeZone: 'UTC', retention: 7,
  sftp: { host: 'sftp.example.test', port: 22, path: '/backups', user: 'ilm', hostKey: fingerprint },
  secretsSet: { ...noSecrets, password: true },
  health: { name: 'sftp', warning: true, lastError: 'authFailed', lastSuccess: '2026-11-10T01:31:00Z' },
};
const nfsTarget = {
  name: 'nas', type: 'nfs', schedule: '0 2 * * *', timeZone: 'UTC', retention: 14,
  nfs: { server: 'nas.example.test', export: '/exports', path: 'ilm', version: '4.1' },
  secretsSet: noSecrets, health: { name: 'nas', warning: false },
};

function backups(over: object = {}) {
  return {
    running: false,
    local: [
      { name: 'appliance-20261120T020000Z-1.0.0.ilmbackup', time: '2026-11-20T02:00:00Z', sizeBytes: 3 * 1024 * 1024, database: 'included' },
      { name: 'appliance-20261119T020000Z-1.0.0.ilmbackup', time: '2026-11-19T02:00:00Z', sizeBytes: 1024, database: 'notIncluded' },
    ],
    targets: [s3Target, sftpTarget, nfsTarget],
    ...over,
  };
}

describe('BackupsPage', () => {
  it('lists the local backups with their downloads and marks a backup without the database', async () => {
    stubApi({ [BACKUPS]: [200, backups()] });
    render(<BackupsPage />);
    const link = await screen.findByRole('link', { name: 'Download appliance-20261120T020000Z-1.0.0.ilmbackup' });
    expect(link).toHaveAttribute('href', '/appliance/api/v1/backups/appliance-20261120T020000Z-1.0.0.ilmbackup');
    expect(link).toHaveAttribute('download');
    expect(screen.getByText(/3\.0 MB/)).toBeInTheDocument();
    expect(screen.getAllByText('Database not included')).toHaveLength(1);
    expect(screen.getByText(/external database must be backed up by its owner/i)).toBeInTheDocument();
  });

  it('reads the backups again until the running one has ended', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout'] });
    let reads = 0;
    stubApi({ [BACKUPS]: () => [200, backups({ running: ++reads < 3, local: reads < 3 ? [] : backups().local })] });
    render(<BackupsPage />);
    expect(await screen.findByText('A backup is running')).toBeInTheDocument();
    for (const read of [2, 3]) {
      await vi.advanceTimersByTimeAsync(runningRefreshMs);
      await vi.waitFor(() => expect(reads).toBe(read));
    }
    expect(await screen.findByText(/3\.0 MB/)).toBeInTheDocument();
    expect(screen.queryByText('A backup is running')).not.toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(3 * runningRefreshMs);
    expect(reads).toBe(3);
    vi.useRealTimers();
  });

  it('says when there are no backups or targets and reports a list that cannot be read', async () => {
    stubApi({ [BACKUPS]: [200, backups({ local: [], targets: [] })] });
    const first = render(<BackupsPage />);
    expect(await screen.findByText('No backup was taken yet.')).toBeInTheDocument();
    expect(screen.getByText('No target receives copies yet.')).toBeInTheDocument();
    first.unmount();
    stubApi({ [BACKUPS]: [409, { code: 'not_set_up', message: 'Setup is not done.' }] });
    render(<BackupsPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup is not done.');
  });

  it('backs up now and shows the backup running', async () => {
    let state = backups();
    stubApi({
      [BACKUPS]: () => [200, state],
      'POST /appliance/api/v1/backups': () => {
        state = backups({ running: true });
        return [202];
      },
    });
    render(<BackupsPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Back up now' }));
    expect(await screen.findByText(/the backup started/i)).toBeInTheDocument();
    expect(await screen.findByText('A backup is running')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Back up now' })).toBeDisabled();
  });

  it('shows why a backup could not start', async () => {
    stubApi({
      [BACKUPS]: [200, backups()],
      'POST /appliance/api/v1/backups': [409, { code: 'no_recovery_kit', message: 'Issue the recovery kit first.' }],
    });
    render(<BackupsPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Back up now' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Issue the recovery kit first.');
  });

  it('shows the targets with their health, last success and last error as text', async () => {
    stubApi({ [BACKUPS]: [200, backups({ healthy: { type: 'BackupHealthy', status: 'False', reason: 'TargetFailed', message: 'sftp: authFailed' } })] });
    render(<BackupsPage />);
    expect(await screen.findByText(/backups are not healthy: sftp: authFailed/i)).toBeInTheDocument();
    const items = screen.getAllByRole('listitem');
    const offsite = items.find((item) => item.textContent?.includes('offsite'))!;
    expect(within(offsite).getByText('Healthy')).toBeInTheDocument();
    expect(within(offsite).getByText('https://s3.example.test · ilm/appliance')).toBeInTheDocument();
    const sftp = items.find((item) => item.textContent?.includes('ilm@sftp.example.test:/backups'))!;
    expect(within(sftp).getByText('Last copy failed')).toBeInTheDocument();
    expect(within(sftp).getByText('Last error: The target refused the credentials.')).toBeInTheDocument();
    expect(within(sftp).getByText(/more than twice the schedule's interval/)).toBeInTheDocument();
    const nas = items.find((item) => item.textContent?.includes('nas.example.test:/exports/ilm'))!;
    expect(within(nas).getByText('No copy yet')).toBeInTheDocument();
    expect(within(nas).getByText('Last success: never')).toBeInTheDocument();
  });

  it('shows an overdue target without an error', async () => {
    stubApi({ [BACKUPS]: [200, backups({ targets: [{ ...s3Target, health: { name: 'offsite', warning: true } }] })] });
    render(<BackupsPage />);
    expect(await screen.findByText('Overdue')).toBeInTheDocument();
  });

  it('tests a target and shows the result', async () => {
    let answer: readonly [number, unknown?] = [200, { ok: true }];
    stubApi({ [BACKUPS]: [200, backups()], 'POST /appliance/api/v1/backups/targets/offsite/test': () => answer });
    render(<BackupsPage />);
    const test = await screen.findByRole('button', { name: 'Test offsite' });
    await userEvent.click(test);
    expect(await screen.findByText(/the test succeeded/i)).toBeInTheDocument();
    answer = [200, { ok: false, reason: 'unreachable' }];
    await userEvent.click(test);
    expect(await screen.findByText(/the test failed\. the target cannot be reached/i)).toBeInTheDocument();
    answer = [404, { code: 'not_found', message: 'No such target.' }];
    await userEvent.click(test);
    expect(await screen.findByText(/the test failed\.$/i)).toBeInTheDocument();
  });

  it('reports a test the appliance did not answer', async () => {
    stubApi({ [BACKUPS]: [200, backups()] });
    render(<BackupsPage />);
    const test = await screen.findByRole('button', { name: 'Test offsite' });
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('offline')));
    await userEvent.click(test);
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('removes a target after confirmation', async () => {
    let items = [s3Target];
    stubApi({
      [BACKUPS]: () => [200, backups({ targets: items })],
      'DELETE /appliance/api/v1/backups/targets/offsite': () => {
        items = [];
        return [202];
      },
    });
    render(<BackupsPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit offsite' }));
    await userEvent.click(screen.getByRole('button', { name: 'Remove offsite' }));
    expect(screen.getByText(/copies already on it stay there/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText('No target receives copies yet.')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Add a target' })).toBeInTheDocument();
  });
});
