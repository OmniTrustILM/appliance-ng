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
  return { available: [], loaded: [], ...over };
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
        loaded: [{ ...platform, version: '2.0.0', loadedAt: '2026-11-20T02:00:00Z' }],
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
