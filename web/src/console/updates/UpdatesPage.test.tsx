import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { hang, stubApi } from '../../setup/test-utils';
import { UpdatesPage, checkTimeoutMs, runningRefreshMs } from './UpdatesPage';

const UPDATES = 'GET /appliance/api/v1/updates';
const MB = 1024 * 1024;

class FakeXhr {
  static last: FakeXhr;
  upload: { onprogress?: (e: { lengthComputable: boolean; loaded: number; total: number }) => void } = {};
  headers: Record<string, string> = {};
  method = '';
  url = '';
  status = 0;
  responseText = '';
  body: unknown;
  onload?: () => void;
  onerror?: () => void;
  constructor() {
    FakeXhr.last = this;
  }
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  setRequestHeader(name: string, value: string) {
    this.headers[name] = value;
  }
  send(body: unknown) {
    this.body = body;
  }
  progress(loaded: number, total: number) {
    this.upload.onprogress?.({ lengthComputable: true, loaded, total });
  }
  answer(status: number, body?: object) {
    this.status = status;
    this.responseText = body ? JSON.stringify(body) : '';
    this.onload?.();
  }
}

const system = { kind: 'system', version: '1.3.0', arch: 'amd64', sizeBytes: 2048 * MB, operatorRange: '>=1.0.0', notes: '/appliance/api/v1/updates/notes/system/1.3.0' };
const platform = { kind: 'platform', version: '2.1.0', arch: 'amd64', ilm: '3.4.0', operator: '1.5.0', sizeBytes: 800 * MB, notes: '/appliance/api/v1/updates/notes/platform/2.1.0' };

function updates(over: object = {}) {
  return { available: [], loaded: [], externalDatabase: false, ...over };
}

async function chooseFile(name = 'bundle.tar', size = 10 * MB) {
  const file = new File(['x'], name);
  Object.defineProperty(file, 'size', { value: size });
  await userEvent.upload(screen.getByLabelText(/bundle file/i), file);
  return file;
}

beforeEach(() => {
  vi.stubGlobal('XMLHttpRequest', FakeXhr);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('UpdatesPage', () => {
  it('lists the available bundles with version, size and kind, and the loaded ones', async () => {
    stubApi({
      [UPDATES]: [200, updates({
        available: [system, platform],
        loaded: [{ ...platform, version: '2.0.0', loadedAt: '2026-11-20T02:00:00Z', blocked: 'sameVersion' }],
        lastCheck: '2026-11-24T10:00:00Z',
      })],
    });
    render(<UpdatesPage />);
    const available = await screen.findByRole('region', { name: 'Available' });
    expect(within(available).getByText('1.3.0')).toBeInTheDocument();
    expect(within(available).getByText(/2\.0 GB/)).toBeInTheDocument();
    expect(within(available).getByText('2.1.0')).toBeInTheDocument();
    expect(within(available).getByText(/ILM 3\.4\.0/)).toBeInTheDocument();
    const loaded = screen.getByRole('region', { name: 'Loaded' });
    expect(within(loaded).getByText('2.0.0')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /install/i })).not.toBeInTheDocument();
  });

  it('says so when nothing is available or loaded', async () => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    expect(await screen.findByText(/no newer bundle is available/i)).toBeInTheDocument();
    expect(screen.getByText(/no bundle is loaded/i)).toBeInTheDocument();
  });

  it.each([
    ['olderThanRunning', undefined, /older than what runs/i],
    ['operatorOutOfRange', undefined, /operator version that runs is outside/i],
    ['needsAppliance', '1.2.0', /install appliance 1\.2\.0 first/i],
    ['wrongArch', undefined, /another architecture/i],
    ['sameVersion', undefined, /already runs this version/i],
    ['invalidMetadata', undefined, /metadata is invalid/i],
    ['somethingNew', undefined, /cannot be installed now/i],
  ])('explains the blocked reason %s', async (blocked, needs, text) => {
    stubApi({ [UPDATES]: [200, updates({ available: [{ ...system, blocked, needs }] })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^fetch/i })).not.toBeInTheDocument();
  });

  it.each([
    [undefined, /bundle's ilm operator version is outside the range the running appliance supports\.$/i],
    ['1.2.0', /bundle's ilm operator version is outside the range the running appliance supports\. install appliance 1\.2\.0 first\./i],
  ])('explains a platform bundle whose operator is out of range, needing %s', async (needs, text) => {
    stubApi({ [UPDATES]: [200, updates({ available: [{ ...platform, blocked: 'operatorOutOfRange', needs }] })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it('explains a platform load refused for its operator', async () => {
    const load = { state: 'refused', kind: 'platform', version: '2.1.0', reason: 'operatorOutOfRange', startedAt: '2026-11-24T10:00:00Z' };
    stubApi({ [UPDATES]: [200, updates({ load })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(/running appliance supports/i)).toBeInTheDocument();
  });

  it.each([
    ['unreachable', /cannot be reached/i],
    ['badSignature', /signature/i],
    ['timeout', /did not answer in time/i],
    ['failed', /check failed/i],
    ['odd', /check failed/i],
  ])('shows the last check error %s', async (lastCheckError, text) => {
    stubApi({ [UPDATES]: [200, updates({ lastCheckError })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it('shows the check as pending until it ends, then the new list', async () => {
    let answer: (v: [number, object]) => void = () => {};
    let list = updates();
    stubApi({
      [UPDATES]: () => [200, list],
      'POST /appliance/api/v1/updates/check': () => new Promise((resolve) => { answer = resolve; }),
    });
    render(<UpdatesPage />);
    const button = await screen.findByRole('button', { name: 'Check now' });
    await userEvent.click(button);
    expect(button).toBeDisabled();
    expect(screen.getByText(/checking the online repository/i)).toBeInTheDocument();
    list = updates({ available: [system], lastCheck: '2026-11-24T10:00:00Z' });
    await act(async () => answer([200, list]));
    expect(await screen.findByText('1.3.0')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
  });

  it('shows a failed check from the list and an unreachable appliance', async () => {
    let list = updates();
    stubApi({
      [UPDATES]: () => [200, list],
      'POST /appliance/api/v1/updates/check': () => {
        list = updates({ lastCheckError: 'unreachable' });
        return [200, list];
      },
    });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('waits for a check as long as the appliance allows one', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout'] });
    const { mock } = stubApi({ [UPDATES]: [200, updates()], 'POST /appliance/api/v1/updates/check': hang });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    const check = mock.mock.calls.map(([request]) => request).find((request) => request.method === 'POST');
    // The appliance answers within its five minutes for the check, 30 seconds for the status and 10 for the answer.
    await vi.advanceTimersByTimeAsync((5 * 60 + 30 + 10) * 1000);
    expect(check?.signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(checkTimeoutMs);
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
    vi.useRealTimers();
  });

  it('reports a check that never reached the appliance', async () => {
    stubApi({ [UPDATES]: [200, updates()], 'POST /appliance/api/v1/updates/check': [503, { code: 'updates_unavailable', message: 'x' }] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText(/could not be checked/i)).toBeInTheDocument();
  });

  it('shows release notes as plain text, never as markup', async () => {
    const notes = '# Heading\n<img src=x onerror=alert(1)>\n**bold**';
    stubApi({
      [UPDATES]: [200, updates({ available: [system] })],
      'GET /appliance/api/v1/updates/notes/system/1.3.0': [200, notes],
    });
    const { container } = render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Release notes of 1.3.0' }));
    const shown = await screen.findByText(/# Heading/);
    expect(shown.textContent).toBe(notes);
    expect(container.querySelector('img')).toBeNull();
    expect(shown.querySelector('h1')).toBeNull();
    expect(container.querySelector('strong')).toBeNull();
  });

  it('says when there are no release notes', async () => {
    stubApi({ [UPDATES]: [200, updates({ available: [system] })] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Release notes of 1.3.0' }));
    expect(await screen.findByText(/no release notes/i)).toBeInTheDocument();
  });

  it('fetches an available bundle and follows its load', async () => {
    let load: object | undefined;
    const { sent } = stubApi({
      [UPDATES]: () => [200, updates({ available: [system], load })],
      'POST /appliance/api/v1/updates/fetch': () => {
        load = { state: 'running', kind: 'system', version: '1.3.0', startedAt: '2026-11-24T10:00:00Z' };
        return [202];
      },
    });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Fetch 1.3.0' }));
    expect(sent['POST /appliance/api/v1/updates/fetch']).toEqual([{ kind: 'system', version: '1.3.0' }]);
    expect(await screen.findByText(/loading system 1\.3\.0/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeDisabled();
  });

  it('offers no fetch of an available bundle that is loaded already', async () => {
    stubApi({ [UPDATES]: [200, updates({ available: [system, platform], loaded: [{ ...system, loadedAt: '2026-11-24T10:00:00Z' }] })] });
    render(<UpdatesPage />);
    expect(await screen.findByRole('button', { name: 'Fetch 2.1.0' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Fetch 1.3.0' })).not.toBeInTheDocument();
  });

  it('explains a fetch that is refused while another operation runs', async () => {
    stubApi({
      [UPDATES]: [200, updates({ available: [system] })],
      'POST /appliance/api/v1/updates/fetch': [409, { code: 'busy', message: 'x' }],
    });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Fetch 1.3.0' }));
    expect(await screen.findByText(/another operation is running/i)).toBeInTheDocument();
  });

  it('keeps reading while a load runs and shows how it ended', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    stubApi({
      [UPDATES]: () => {
        calls++;
        const state = calls < 2 ? 'running' : 'loaded';
        return [200, updates({ load: { state, kind: 'platform', version: '2.1.0', startedAt: '2026-11-24T10:00:00Z' } })];
      },
    });
    render(<UpdatesPage />);
    expect(await screen.findByText(/loading platform 2\.1\.0/i)).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(runningRefreshMs); });
    expect(await screen.findByText(/platform 2\.1\.0 was loaded/i)).toBeInTheDocument();
  });

  it('marks only the reads it makes on its own as background refreshes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    const { mock } = stubApi({
      [UPDATES]: () => {
        calls++;
        const state = calls < 3 ? 'running' : 'loaded';
        return [200, updates({ load: { state, kind: 'platform', version: '2.1.0', startedAt: '2026-11-24T10:00:00Z' } })];
      },
      'POST /appliance/api/v1/updates/check': [200, updates()],
    });
    render(<UpdatesPage />);
    expect(await screen.findByText(/loading platform 2\.1\.0/i)).toBeInTheDocument();
    for (const call of [2, 3]) {
      await act(async () => { await vi.advanceTimersByTimeAsync(runningRefreshMs); });
      await vi.waitFor(() => expect(calls).toBe(call));
    }
    expect(await screen.findByText(/platform 2\.1\.0 was loaded/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Check now' }));
    await vi.waitFor(() => expect(calls).toBe(4));
    const reads = mock.mock.calls.map(([request]) => request).filter((request) => request.method === 'GET' && request.url.endsWith('/updates'));
    expect(reads.map((request) => request.headers.get('X-Background-Refresh'))).toEqual([null, '1', '1', null]);
  });

  it.each([
    ['unsigned', /not signed/i],
    ['badSignature', /signature/i],
    ['damaged', /damaged/i],
    ['noSpace', /free space/i],
    ['wrongKind', /other kind/i],
    ['unmirroredRegistry', /does not mirror/i],
    ['olderThanRunning', /older than what runs/i],
    ['strange', /could not be loaded/i],
  ])('explains a refused load with the reason %s', async (reason, text) => {
    stubApi({ [UPDATES]: [200, updates({ load: { state: 'refused', kind: 'system', reason, startedAt: '2026-11-24T10:00:00Z' } })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it('uploads a bundle with its progress and then follows the load', async () => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    await screen.findByText(/no newer bundle/i);
    await userEvent.selectOptions(screen.getByLabelText('Kind'), 'platform');
    const file = await chooseFile();
    await userEvent.click(screen.getByRole('button', { name: 'Upload' }));
    const xhr = FakeXhr.last;
    expect(xhr.method).toBe('PUT');
    expect(xhr.url).toBe('/appliance/api/v1/updates/upload?kind=platform');
    expect(xhr.body).toBe(file);
    act(() => xhr.progress(5 * MB, 10 * MB));
    const bar = screen.getByRole('progressbar', { name: 'Upload progress' });
    expect(bar).toHaveAttribute('value', String(5 * MB));
    expect(bar).toHaveAttribute('max', String(10 * MB));
    expect(screen.getByText(/50%/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Upload' })).toBeDisabled();
    await act(async () => xhr.answer(202));
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
    expect(screen.getByText(/uploaded; it is being checked and loaded/i)).toBeInTheDocument();
  });

  it.each([
    [409, 'no_space', /three times the bundle's size/i],
    [409, 'busy', /another operation is running/i],
    [409, 'bundle_loading', /another operation is running/i],
    [400, 'incomplete', /did not arrive completely/i],
    [503, 'updates_unavailable', /not available/i],
    [500, 'odd', /could not be uploaded/i],
  ])('explains an upload refused with %i %s', async (status, code, text) => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    await screen.findByText(/no newer bundle/i);
    await chooseFile();
    await userEvent.click(screen.getByRole('button', { name: 'Upload' }));
    await act(async () => FakeXhr.last.answer(status, { code, message: 'x' }));
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Upload' })).toBeEnabled();
  });

  it('keeps the chosen file while it uploads', async () => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    await screen.findByText(/no newer bundle/i);
    await chooseFile('first.tar');
    await userEvent.click(screen.getByRole('button', { name: 'Upload' }));
    expect(screen.getByLabelText(/bundle file/i)).toBeDisabled();
    fireEvent.drop(screen.getByTestId('drop-zone'), { dataTransfer: { files: [new File(['y'], 'second.tar')] } });
    expect(screen.getByText('first.tar')).toBeInTheDocument();
    await act(async () => FakeXhr.last.answer(409, { code: 'busy', message: 'x' }));
    expect(screen.getByLabelText(/bundle file/i)).toBeEnabled();
    expect(screen.getByText('first.tar')).toBeInTheDocument();
  });

  it('reports an upload that broke off', async () => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    await screen.findByText(/no newer bundle/i);
    await chooseFile();
    await userEvent.click(screen.getByRole('button', { name: 'Upload' }));
    await act(async () => FakeXhr.last.onerror?.());
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('keeps Upload disabled until a file is chosen', async () => {
    stubApi({ [UPDATES]: [200, updates()] });
    render(<UpdatesPage />);
    expect(await screen.findByRole('button', { name: 'Upload' })).toBeDisabled();
  });

  it('shows an error when the updates cannot be read', async () => {
    stubApi({ [UPDATES]: [503, { code: 'updates_unavailable', message: 'Update bundles are not available in this manager.' }] });
    render(<UpdatesPage />);
    expect(await screen.findByText(/not available in this manager/i)).toBeInTheDocument();
  });

  it('waits while the first read has not answered', () => {
    stubApi({ [UPDATES]: hang });
    render(<UpdatesPage />);
    expect(screen.getByRole('status')).toBeInTheDocument();
  });
});

const PLATFORM = 'POST /appliance/api/v1/updates/platform';
const loadedPlatform = { ...platform, loadedAt: '2026-11-24T10:00:00Z' };

function progress(over: object = {}) {
  return { kind: 'platform', from: '2.0.0', to: '2.1.0', step: 'backup', startedAt: '2026-11-24T10:00:00Z', pointOfNoReturn: false, ...over };
}

describe('UpdatesPage platform update', () => {
  it('offers Install only for a loaded platform bundle that can be installed', async () => {
    stubApi({
      [UPDATES]: [200, updates({
        available: [platform],
        loaded: [loadedPlatform, { ...system, loadedAt: '2026-11-24T10:00:00Z' }, { ...loadedPlatform, version: '1.9.0', blocked: 'olderThanRunning' }],
      })],
    });
    render(<UpdatesPage />);
    expect(await screen.findByRole('button', { name: 'Install 2.1.0' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^install/i })).toHaveLength(1);
  });

  it('says before the update that a backup comes first and that the way back loses the data since', async () => {
    const { sent } = stubApi({ [UPDATES]: [200, updates({ loaded: [loadedPlatform], operator: 'v1.5.0' })] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    const dialog = screen.getByRole('dialog', { name: /install platform 2\.1\.0/i });
    expect(within(dialog).getByText(/ILM 3\.4\.0/)).toBeInTheDocument();
    expect(within(dialog).getByText(/takes a backup to every backup target first/i)).toBeInTheDocument();
    expect(within(dialog).getByText(/the update stops if the backup fails/i)).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/rolled back: ILM is installed again at the previous version and that backup is restored/i);
    expect(dialog).toHaveTextContent(/data changed after the backup is lost/i);
    expect(dialog).toHaveTextContent(/needs the recovery kit and its passphrase/i);
    expect(dialog).not.toHaveTextContent(/fresh appliance/i);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(sent[PLATFORM]).toBeUndefined();
  });

  it('says that the way back from a bundle with a newer ILM operator is a restore onto a fresh appliance', async () => {
    stubApi({ [UPDATES]: [200, updates({ loaded: [loadedPlatform], operator: 'v1.4.2' })] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    const dialog = screen.getByRole('dialog', { name: /install platform 2\.1\.0/i });
    expect(dialog).toHaveTextContent(/newer ILM operator, which is never downgraded/i);
    expect(dialog).toHaveTextContent(/restore that backup onto a fresh appliance/i);
    expect(dialog).not.toHaveTextContent(/rolled back/i);
  });

  it('says that the way back with an external database is a restore onto a fresh appliance', async () => {
    stubApi({ [UPDATES]: [200, updates({ loaded: [loadedPlatform], operator: 'v1.5.0', externalDatabase: true })] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    const dialog = screen.getByRole('dialog', { name: /install platform 2\.1\.0/i });
    expect(dialog).toHaveTextContent(/uses an external database/i);
    expect(dialog).toHaveTextContent(/restore that backup onto a fresh appliance/i);
    expect(dialog).not.toHaveTextContent(/rolled back/i);
  });

  it('promises no rollback while the running ILM operator is not known', async () => {
    stubApi({ [UPDATES]: [200, updates({ loaded: [loadedPlatform] })] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    const dialog = screen.getByRole('dialog', { name: /install platform 2\.1\.0/i });
    expect(dialog).toHaveTextContent(/ILM operator that runs is not known/i);
    expect(dialog).toHaveTextContent(/restore that backup onto a fresh appliance/i);
    expect(dialog).not.toHaveTextContent(/rolled back/i);
  });

  it('starts the confirmed update and follows its progress', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let update: object | undefined;
    const { mock, sent } = stubApi({
      [UPDATES]: () => [200, updates({ loaded: [loadedPlatform], update })],
      [PLATFORM]: () => {
        update = progress();
        return [202];
      },
    });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Install' }));
    expect(sent[PLATFORM]).toEqual([{ version: '2.1.0', confirm: true }]);
    expect(await screen.findByText(/updating the platform from 2\.0\.0 to 2\.1\.0/i)).toBeInTheDocument();
    expect(screen.getByText(/taking a backup to every target/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Install 2.1.0' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeDisabled();
    update = progress({ step: 'wait', backup: 'backup-1', pointOfNoReturn: true });
    await act(async () => { await vi.advanceTimersByTimeAsync(runningRefreshMs); });
    expect(await screen.findByText(/waiting for ILM to run the new version/i)).toBeInTheDocument();
    update = progress({ step: 'done', backup: 'backup-1' });
    await act(async () => { await vi.advanceTimersByTimeAsync(runningRefreshMs); });
    expect(await screen.findByText(/the platform was updated to 2\.1\.0/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Install 2.1.0' })).toBeEnabled();
    // The reads that follow the update are the page's own, which must not keep the session from going idle.
    const reads = mock.mock.calls.map(([request]) => request).filter((request) => request.method === 'GET' && request.url.endsWith('/updates'));
    expect(reads.slice(0, 2).map((request) => request.headers.get('X-Background-Refresh'))).toEqual([null, null]);
    expect(reads.slice(2).map((request) => request.headers.get('X-Background-Refresh'))).toEqual(reads.slice(2).map(() => '1'));
    expect(reads.length).toBeGreaterThan(2);
  });

  it.each([
    ['images', 'imagesMissing', /failed while making the bundle's images available.*images are missing/i],
    ['backup', 'backupFailed', /failed while taking a backup.*backup to every target did not complete/i],
    ['backup', 'backupIncomplete', /failed while taking a backup.*backup does not include the database/i],
    ['wait', 'timeout', /did not finish in time/i],
    ['operator', 'strange', /the manager log has the details/i],
    ['version', 'platformUpgrading', /operator was upgrading the platform, so the new ILM version was not asked for/i],
    ['version', 'notInstalled', /platform is not installed/i],
    ['wait', 'failed', /a step failed\. the manager log has the details/i],
  ])('explains an update that failed at %s with %s', async (step, message, text) => {
    stubApi({ [UPDATES]: [200, updates({ update: progress({ step, message, backup: 'backup-1' }) })] });
    render(<UpdatesPage />);
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.getByText(/backup-1/)).toBeInTheDocument();
  });

  it.each([
    [409, 'update_running', /another update is running/i],
    [409, 'backup_running', /a backup is running/i],
    [409, 'restore_running', /a restore is running/i],
    [409, 'bundle_loading', /another operation is running/i],
    [409, 'busy', /another operation is running/i],
    [409, 'upgrade_in_progress', /operator is upgrading the platform/i],
    [409, 'not_installed', /platform is not installed/i],
    [409, 'last_update_failed', /last platform update failed\. roll it back/i],
    [409, 'operator_older_than_running', /ILM operator is older than the one that runs/i],
    [409, 'blocked', /cannot be installed on what runs/i],
    [409, 'no_space', /twice the bundle's size/i],
    [404, 'not_found', /neither loaded nor found online/i],
    [500, 'internal', /could not be started/i],
  ])('explains an update refused with %i %s', async (status, code, text) => {
    stubApi({ [UPDATES]: [200, updates({ loaded: [loadedPlatform] })], [PLATFORM]: [status, { code, message: 'x' }] });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Install' }));
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('reports an update request that never reached the appliance', async () => {
    stubApi({ [UPDATES]: [200, updates({ loaded: [loadedPlatform] })], [PLATFORM]: () => Promise.reject(new TypeError('offline')) });
    render(<UpdatesPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Install 2.1.0' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Install' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });
});
