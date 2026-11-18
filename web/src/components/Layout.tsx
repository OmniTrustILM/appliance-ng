import type { ReactNode } from 'react';

export function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="layout">
      <header className="layout-header">ILM Appliance</header>
      <main className="layout-main">{children}</main>
    </div>
  );
}
