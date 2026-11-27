import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { setCsrfToken } from '../api/client';
import { stubApi } from '../setup/test-utils';
import { SignInPage } from './SignInPage';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  setCsrfToken(undefined);
});

function renderPage(returnTo?: string) {
  return render(
    <MemoryRouter initialEntries={[{ pathname: '/signin', state: returnTo ? { returnTo } : null }]}>
      <Routes>
        <Route path="/updates" element={<p>updates page</p>} />
        <Route path="/signin" element={<SignInPage />} />
        <Route path="/" element={<p>console page</p>} />
        <Route path="/wizard/" element={<p>wizard page</p>} />
        <Route path="/recovery-kit" element={<p>recovery kit page</p>} />
        <Route path="/restore" element={<p>restore page</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function fill(totp = '123 456') {
  await userEvent.type(screen.getByLabelText('User name'), 'admin');
  await userEvent.type(screen.getByLabelText('Password'), 'correct horse battery');
  await userEvent.type(screen.getByLabelText('Authenticator code'), totp);
  await userEvent.click(screen.getByRole('button', { name: 'Sign in' }));
}

describe('SignInPage', () => {
  it('shows the sign-in heading', () => {
    renderPage();
    expect(screen.getByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });

  it('signs in with user, password and code and opens the wizard while setup is open', async () => {
    const { sent } = stubApi({
      'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
      'GET /appliance/api/v1/status': [200, { setup: 'part2', conditions: [], versions: {}, rebootRequired: false }],
    });
    renderPage();
    await fill();
    expect(await screen.findByText('wizard page')).toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/session']).toEqual([{ user: 'admin', password: 'correct horse battery', totp: '123 456' }]);
    expect(location.href).not.toContain('correct');
  });

  it('opens the status page once setup is complete', async () => {
    stubApi({
      'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
      'GET /appliance/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }],
    });
    renderPage();
    await fill();
    expect(await screen.findByText('console page')).toBeInTheDocument();
  });

  it('sends an administrator without a confirmed recovery kit to the recovery kit page first', async () => {
    for (const kit of [{ issued: false, confirmed: false }, { issued: true, confirmed: false }]) {
      stubApi({
        'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
        'GET /appliance/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }],
        'GET /appliance/api/v1/recovery-kit': [200, kit],
      });
      const page = renderPage();
      await fill();
      expect(await screen.findByText('recovery kit page')).toBeInTheDocument();
      page.unmount();
    }
  });

  it('opens the restore page while the appliance waits for a backup, not the recovery kit prompt', async () => {
    const { sent } = stubApi({
      'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
      'GET /appliance/api/v1/status': [200, { setup: 'restore', conditions: [], versions: {}, rebootRequired: false }],
      'GET /appliance/api/v1/recovery-kit': [200, { issued: false, confirmed: false }],
    });
    renderPage();
    await fill();
    expect(await screen.findByText('restore page')).toBeInTheDocument();
    expect(sent['GET /appliance/api/v1/recovery-kit']).toBeUndefined();
  });

  it('opens the restore page when a restore in place did not finish', async () => {
    for (const restore of [
      { backup: 'b.ilmbackup', inPlace: true, step: 'waitingForKit', pausedStep: 'database' },
      { backup: 'b.ilmbackup', inPlace: true, step: 'database', message: 'failed' },
    ]) {
      stubApi({
        'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
        'GET /appliance/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }],
        'GET /appliance/api/v1/restore': [200, { setup: 'complete', restore }],
        'GET /appliance/api/v1/recovery-kit': [200, { confirmed: false }],
      });
      const { unmount } = renderPage();
      await fill();
      expect(await screen.findByText('restore page')).toBeInTheDocument();
      unmount();
    }
  });

  it('opens the status page when the recovery kit is confirmed', async () => {
    stubApi({
      'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
      'GET /appliance/api/v1/status': [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired: false }],
      'GET /appliance/api/v1/recovery-kit': [200, { issued: true, confirmed: true }],
    });
    renderPage();
    await fill();
    expect(await screen.findByText('console page')).toBeInTheDocument();
  });

  it('opens the page whose session ended again, unless setup or a restore needs another', async () => {
    for (const [setup, page] of [['complete', 'updates page'], ['part2', 'wizard page']]) {
      stubApi({
        'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }],
        'GET /appliance/api/v1/status': [200, { setup, conditions: [], versions: {}, rebootRequired: false }],
        'GET /appliance/api/v1/recovery-kit': [200, { issued: true, confirmed: true }],
      });
      const { unmount } = renderPage('/updates');
      await fill();
      expect(await screen.findByText(page)).toBeInTheDocument();
      unmount();
    }
  });

  it('opens the status page when the status cannot be read', async () => {
    stubApi({ 'POST /appliance/api/v1/session': [200, { csrfToken: 'token-1' }] });
    renderPage();
    await fill();
    expect(await screen.findByText('console page')).toBeInTheDocument();
  });

  it('gives one message for a failed sign-in with the appliance time and clears the secrets', async () => {
    stubApi({
      'POST /appliance/api/v1/session': [400, { code: 'invalid_credentials', message: 'x', serverTime: '2026-11-18T10:00:00Z' }],
    });
    renderPage();
    await fill();
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('The user name, password or code is not correct.');
    expect(alert).toHaveTextContent('2026-11-18T10:00:00Z');
    expect(alert).not.toHaveTextContent(/which|wrong password|wrong code/i);
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(screen.getByLabelText('Authenticator code')).toHaveValue('');
    expect(screen.getByLabelText('User name')).toHaveValue('admin');
  });

  it('counts down a lock-out and then allows another attempt', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    stubApi({ 'POST /appliance/api/v1/session': [401, { code: 'locked', message: 'x', retryAfter: 3 }] });
    renderPage();
    await userEvent.setup({ advanceTimers: vi.advanceTimersByTime }).type(screen.getByLabelText('User name'), 'admin');
    await userEvent.setup({ advanceTimers: vi.advanceTimersByTime }).click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Try again in 3 seconds');
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeDisabled();
    for (let second = 0; second < 3; second++) await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(screen.queryByText(/Try again in/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled();
  });

  it('says so when the appliance cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('offline'); }));
    renderPage();
    await fill();
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });

  it('shows a generic message for any other answer', async () => {
    stubApi({ 'POST /appliance/api/v1/session': [500, { code: 'internal', message: 'x' }] });
    renderPage();
    await fill();
    expect(await screen.findByRole('alert')).toHaveTextContent('could not process');
  });
});
