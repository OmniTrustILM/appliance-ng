import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, setCsrfToken } from './api/client';
import { SessionProvider, useSession, useSessionControl, type SignInState } from './session';
import { stubApi } from './setup/test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(undefined);
});

function Probe() {
  const session = useSession();
  const { signIn, signOut } = useSessionControl();
  return (
    <>
      <p>{session.state === 'signedIn' ? `signed in as ${session.user}` : session.state}</p>
      <button type="button" onClick={() => signIn('admin', 'token-1')}>sign in</button>
      <button type="button" onClick={() => signOut()}>sign out</button>
    </>
  );
}

/** Stubs fetch so that the initial session lookup answers only when the returned function is called, and waits until
 * the answer is applied. */
function pendingLookup() {
  let answer: ((response: Response) => void) | undefined;
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>((resolve) => { answer = resolve; })));
  return (status: number, body: unknown) =>
    act(async () => {
      await vi.waitFor(() => expect(answer).toBeDefined());
      answer!(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
      // Lets the client read the answer and the provider apply it.
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
}

function renderProbe() {
  render(<MemoryRouter><SessionProvider><Probe /></SessionProvider></MemoryRouter>);
}

describe('SessionProvider', () => {
  it('keeps a sign-in made while the initial lookup was pending', async () => {
    const answer = pendingLookup();
    renderProbe();
    expect(screen.getByText('loading')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'sign in' }));
    await answer(401, { code: 'unauthorized', message: 'x' });
    expect(screen.getByText('signed in as admin')).toBeInTheDocument();
  });

  it('keeps a sign-out made while the initial lookup was pending', async () => {
    const answer = pendingLookup();
    renderProbe();
    await userEvent.click(screen.getByRole('button', { name: 'sign out' }));
    await answer(200, { user: 'admin', csrfToken: 'token-0' });
    expect(screen.getByText('signedOut')).toBeInTheDocument();
  });

  it('takes the session from the lookup when nothing changed meanwhile', async () => {
    const answer = pendingLookup();
    renderProbe();
    await answer(200, { user: 'admin', csrfToken: 'token-0' });
    expect(screen.getByText('signed in as admin')).toBeInTheDocument();
  });

  it('sends a request that finds the session ended to sign-in, to come back to its page', async () => {
    stubApi({
      'GET /appliance/api/v1/session': [200, { user: 'admin', csrfToken: 'token-0' }],
      'GET /appliance/api/v1/updates': [401, { code: 'unauthorized', message: 'x' }],
    });
    function SignIn() {
      const state = useLocation().state as SignInState | null;
      return <p>sign-in returns to {state?.returnTo}</p>;
    }
    render(
      <MemoryRouter initialEntries={['/updates']}>
        <SessionProvider>
          <Routes>
            <Route path="/updates" element={<button type="button" onClick={() => void api.GET('/updates')}>read</button>} />
            <Route path="/signin" element={<SignIn />} />
          </Routes>
        </SessionProvider>
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole('button', { name: 'read' }));
    expect(await screen.findByText('sign-in returns to /updates')).toBeInTheDocument();
  });

  it('keeps the page to come back to when a further request finds the session ended', async () => {
    stubApi({
      'GET /appliance/api/v1/session': [200, { user: 'admin', csrfToken: 'token-0' }],
      'GET /appliance/api/v1/updates': [401, { code: 'unauthorized', message: 'x' }],
    });
    function SignIn() {
      const state = useLocation().state as SignInState | null;
      return <p>sign-in returns to {state?.returnTo ?? 'nothing'}</p>;
    }
    render(
      <MemoryRouter initialEntries={['/updates']}>
        <SessionProvider>
          <Routes>
            <Route path="/updates" element={<button type="button" onClick={() => void api.GET('/updates')}>read</button>} />
            <Route path="/signin" element={<SignIn />} />
          </Routes>
        </SessionProvider>
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole('button', { name: 'read' }));
    expect(await screen.findByText('sign-in returns to /updates')).toBeInTheDocument();
    await act(async () => {
      await api.GET('/updates');
    });
    expect(screen.getByText('sign-in returns to /updates')).toBeInTheDocument();
  });
});
