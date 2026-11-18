import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { setCsrfToken } from './api/client';
import { SessionProvider, useSession, useSessionControl } from './session';

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
});
