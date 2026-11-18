import type { ReactNode } from 'react';
import { Navigate, Outlet, Route, Routes } from 'react-router';
import { Layout } from './components/Layout';
import { StatusPage } from './console/status/StatusPage';
import { RequireSession, SessionProvider } from './session';
import { SetupFlow } from './setup/SetupFlow';
import { SignInPage } from './signin/SignInPage';
import { DonePage } from './wizard/DonePage';
import { WizardFlow } from './wizard/WizardFlow';

function ManagerRoutes() {
  return (
    <SessionProvider>
      <Outlet />
    </SessionProvider>
  );
}

function ConsolePage({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <Layout variant="console">
      <RequireSession>{children}</RequireSession>
    </Layout>
  );
}

export default function App() {
  return (
    <Routes>
      <Route path="/setup/*" element={<Layout variant="setup"><SetupFlow /></Layout>} />
      <Route element={<ManagerRoutes />}>
        <Route path="/signin" element={<Layout variant="auth"><SignInPage /></Layout>} />
        <Route path="/wizard/done" element={<ConsolePage><DonePage /></ConsolePage>} />
        <Route path="/wizard/*" element={<ConsolePage><WizardFlow /></ConsolePage>} />
        <Route path="/" element={<ConsolePage><StatusPage /></ConsolePage>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
