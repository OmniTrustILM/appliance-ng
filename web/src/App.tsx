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

export default function App() {
  return (
    <Layout>
      <Routes>
        <Route path="/setup/*" element={<SetupFlow />} />
        <Route element={<ManagerRoutes />}>
          <Route path="/signin" element={<SignInPage />} />
          <Route path="/wizard/done" element={<RequireSession><DonePage /></RequireSession>} />
          <Route path="/wizard/*" element={<RequireSession><WizardFlow /></RequireSession>} />
          <Route path="/" element={<RequireSession><StatusPage /></RequireSession>} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </Layout>
  );
}
