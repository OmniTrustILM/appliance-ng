import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Navigate, useNavigate } from 'react-router';
import { Loading } from './components/Spinner';
import { api, setCsrfToken, setUnauthorizedHandler } from './api/client';
import { attempt } from './setup/failure';

type Session = { state: 'loading' } | { state: 'signedOut' } | { state: 'signedIn'; user: string };

type SessionControl = {
  session: Session;
  /** Records a sign-in: the user and the CSRF token that the API client sends from now on. */
  signIn: (user: string, csrfToken: string) => void;
  /** Records the end of the session. */
  signOut: () => void;
};

const SessionContext = createContext<SessionControl>({
  session: { state: 'loading' },
  signIn: () => {},
  signOut: () => {},
});

/** Loads the console session once and sends the user to sign-in when the session ends. */
export function SessionProvider({ children }: Readonly<{ children: ReactNode }>) {
  const [session, setSession] = useState<Session>({ state: 'loading' });
  const navigate = useNavigate();
  // navigate changes with every page, which must not load the session again.
  const navigateRef = useRef(navigate);
  useEffect(() => {
    navigateRef.current = navigate;
  }, [navigate]);

  // Counts the changes of the session, so that the initial lookup does not undo a sign-in or sign-out made while it
  // was pending.
  const changes = useRef(0);

  const signIn = useCallback((user: string, csrfToken: string) => {
    changes.current++;
    setCsrfToken(csrfToken);
    setSession({ state: 'signedIn', user });
  }, []);
  const signOut = useCallback(() => {
    changes.current++;
    setCsrfToken(undefined);
    setSession({ state: 'signedOut' });
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(() => {
      changes.current++;
      setCsrfToken(undefined);
      setSession({ state: 'signedOut' });
      void navigateRef.current('/signin');
    });
    let active = true;
    const before = changes.current;
    void attempt(() => api.GET('/session')).then((result) => {
      if (!active || changes.current !== before) return;
      const data = result?.data;
      setCsrfToken(data?.csrfToken);
      setSession(data ? { state: 'signedIn', user: data.user } : { state: 'signedOut' });
    });
    return () => {
      active = false;
    };
  }, []);

  const control = useMemo(() => ({ session, signIn, signOut }), [session, signIn, signOut]);
  return <SessionContext.Provider value={control}>{children}</SessionContext.Provider>;
}

export function useSession() {
  return useContext(SessionContext).session;
}

/** The functions that record a sign-in and a sign-out. */
export function useSessionControl() {
  const { signIn, signOut } = useContext(SessionContext);
  return { signIn, signOut };
}

/** Shows children to a signed-in user and sends anyone else to sign-in. */
export function RequireSession({ children }: Readonly<{ children: ReactNode }>) {
  const session = useSession();
  if (session.state === 'loading') return <Loading />;
  if (session.state === 'signedOut') return <Navigate to="/signin" replace />;
  return children;
}
