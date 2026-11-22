import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { setCsrfToken } from '../api/client';
import { SessionProvider } from '../session';
import { stubApi } from '../setup/test-utils';
import { SignOutButton } from './SignOutButton';

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(undefined);
});

function renderButton() {
  return render(
    <MemoryRouter initialEntries={['/']}>
      <SessionProvider>
        <Routes>
          <Route path="/" element={<SignOutButton />} />
          <Route path="/signin" element={<p>sign-in page</p>} />
        </Routes>
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe('SignOutButton', () => {
  it('ends the session with the CSRF token and returns to sign-in', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/session': [200, { user: 'admin', csrfToken: 'token-9' }],
      'DELETE /appliance/api/v1/session': [204],
    });
    renderButton();
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    expect(await screen.findByText('sign-in page')).toBeInTheDocument();
    const del = mock.mock.calls.map(([r]) => r).find((r) => r.method === 'DELETE')!;
    expect(del.headers.get('X-CSRF-Token')).toBe('token-9');
  });

  it('returns to sign-in even when the session had already ended', async () => {
    stubApi({
      'GET /appliance/api/v1/session': [200, { user: 'admin', csrfToken: 'token-9' }],
      'DELETE /appliance/api/v1/session': [401, { code: 'unauthorized', message: 'x' }],
    });
    renderButton();
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    expect(await screen.findByText('sign-in page')).toBeInTheDocument();
  });

  it.each([
    ['refuses', () => [403, { code: 'csrf', message: 'x' }] as const, 'refused'],
    ['fails', () => [500, { code: 'internal', message: 'x' }] as const, 'could not process'],
  ])('keeps the session and offers a retry when the appliance %s', async (_, reply, message) => {
    let deletes = 0;
    stubApi({
      'GET /appliance/api/v1/session': [200, { user: 'admin', csrfToken: 'token-9' }],
      'DELETE /appliance/api/v1/session': () => (++deletes === 1 ? reply() : [204]),
    });
    renderButton();
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(message);
    expect(screen.queryByText('sign-in page')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(await screen.findByText('sign-in page')).toBeInTheDocument();
  });

  it('keeps the session when the appliance cannot be reached', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/session': [200, { user: 'admin', csrfToken: 'token-9' }] });
    renderButton();
    const button = await screen.findByRole('button', { name: 'Sign out' });
    mock.mockRejectedValueOnce(new TypeError('network'));
    await userEvent.click(button);
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    expect(screen.queryByText('sign-in page')).not.toBeInTheDocument();
  });
});
