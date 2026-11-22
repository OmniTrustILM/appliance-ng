import type { ReactNode } from 'react';
import { Navigate, Outlet, Route, Routes } from 'react-router';
import { Layout } from './components/Layout';
import { AccessPage } from './console/access/AccessPage';
import { AuditPage } from './console/audit/AuditPage';
import { CertificatePage } from './console/certificate/CertificatePage';
import { NodePage } from './console/node/NodePage';
import { MailPage } from './console/mail/MailPage';
import { PlatformPage } from './console/platform/PlatformPage';
import { RecoveryKitPage } from './console/recovery/RecoveryKitPage';
import { RegistriesPage } from './console/registries/RegistriesPage';
import { DashboardPage } from './console/dashboard/DashboardPage';
import { SystemPage } from './console/system/SystemPage';
import { TrustPage } from './console/trust/TrustPage';
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
        <Route path="/node" element={<ConsolePage><NodePage /></ConsolePage>} />
        <Route path="/certificate" element={<ConsolePage><CertificatePage /></ConsolePage>} />
        <Route path="/trust" element={<ConsolePage><TrustPage /></ConsolePage>} />
        <Route path="/registries" element={<ConsolePage><RegistriesPage /></ConsolePage>} />
        <Route path="/platform" element={<ConsolePage><PlatformPage /></ConsolePage>} />
        <Route path="/mail" element={<ConsolePage><MailPage /></ConsolePage>} />
        <Route path="/access" element={<ConsolePage><AccessPage /></ConsolePage>} />
        <Route path="/recovery-kit" element={<ConsolePage><RecoveryKitPage /></ConsolePage>} />
        <Route path="/system" element={<ConsolePage><SystemPage /></ConsolePage>} />
        <Route path="/audit" element={<ConsolePage><AuditPage /></ConsolePage>} />
        <Route path="/" element={<ConsolePage><DashboardPage /></ConsolePage>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
