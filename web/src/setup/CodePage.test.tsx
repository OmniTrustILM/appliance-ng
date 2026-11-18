import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, setCsrfToken } from '../api/client';
import { CodePage } from './CodePage';
import { stubApi } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  setCsrfToken(undefined);
});

describe('CodePage', () => {
  beforeEach(() => setCsrfToken(undefined));

  it('formats the code while it is typed', async () => {
    stubApi({});
    render(<CodePage onDone={vi.fn()} />);
    await userEvent.type(screen.getByLabelText('Setup code'), 'abcdefghjk');
    expect(screen.getByLabelText('Setup code')).toHaveValue('ABCDE-FGHJK');
  });

  it('sends the code, keeps the CSRF token in memory and moves on', async () => {
    const { mock, sent } = stubApi({ 'POST /api/v1/setup/code': [200, { csrfToken: 'tok-1' }] });
    const onDone = vi.fn();
    render(<CodePage onDone={onDone} />);
    await userEvent.type(screen.getByLabelText('Setup code'), 'abcdefghjk');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(sent['POST /api/v1/setup/code']).toEqual([{ code: 'ABCDE-FGHJK' }]);
    expect(mock).toHaveBeenCalledTimes(1);
    expect(localStorage.length + sessionStorage.length).toBe(0);
    // The token is now sent on the next state-changing call.
    const next = stubApi({ 'PUT /api/v1/setup/admin': [204] });
    await api.PUT('/api/v1/setup/admin', { body: { user: 'a', password: 'b' } });
    const request = next.mock.mock.calls[0][0];
    expect(request.headers.get('X-CSRF-Token')).toBe('tok-1');
  });

  it('says the code is wrong', async () => {
    stubApi({ 'POST /api/v1/setup/code': [401, { code: 'invalid_code', message: 'x' }] });
    const onDone = vi.fn();
    render(<CodePage onDone={onDone} />);
    await userEvent.type(screen.getByLabelText('Setup code'), 'abcdefghjk');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('not correct');
    expect(onDone).not.toHaveBeenCalled();
  });

  it('counts the lock-out down and enables submit again at zero', async () => {
    stubApi({ 'POST /api/v1/setup/code': [401, { code: 'invalid_code', message: 'x', retryAfter: 3 }] });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<CodePage onDone={vi.fn()} />);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    await user.type(screen.getByLabelText('Setup code'), 'abcdefghjk');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByText(/Try again in 3 seconds/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(screen.getByText(/Try again in 2 seconds/)).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(1000));
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(screen.queryByText(/Try again in/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeEnabled();
  });

  it('says setup runs in another browser on 409', async () => {
    stubApi({ 'POST /api/v1/setup/code': [409, { code: 'conflict', message: 'x' }] });
    render(<CodePage onDone={vi.fn()} />);
    await userEvent.type(screen.getByLabelText('Setup code'), 'abcdefghjk');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('setup is in progress elsewhere');
  });

  it('shows a generic message when the appliance cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network'); }));
    render(<CodePage onDone={vi.fn()} />);
    await userEvent.type(screen.getByLabelText('Setup code'), 'abcdefghjk');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });
});
