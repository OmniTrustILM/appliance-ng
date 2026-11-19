import type { ReactNode } from 'react';
import ilmWhite from '../assets/brand/ilm-white.svg';
import otColor from '../assets/brand/ot-color.svg';
import otWhite from '../assets/brand/ot-white.svg';
import { ConsoleNav } from './ConsoleNav';
import { useSession } from '../session';
import { SignOutButton } from '../signin/SignOutButton';
import ThemeToggle from '../theme/ThemeToggle';

type LayoutProps = {
  variant: 'setup' | 'console' | 'auth';
  children: ReactNode;
};

function SkipLink() {
  return (
    <a
      href="#main"
      className="sr-only focus:not-sr-only focus:fixed focus:left-3 focus:top-3 focus:z-50 focus:rounded-md focus:bg-surface-raised focus:px-4 focus:py-2 focus:text-sm focus:text-content focus:shadow-card"
    >
      Skip to content
    </a>
  );
}

function UserArea() {
  const session = useSession();
  if (session.state !== 'signedIn') return null;
  return (
    <>
      <span className="hidden min-w-0 truncate text-[13px] sm:inline">{session.user}</span>
      <SignOutButton />
    </>
  );
}

/** The blue top bar of the setup and console shells. */
function TopBar({ product, withUser }: Readonly<{ product: string; withUser: boolean }>) {
  return (
    <header className="relative flex h-[52px] items-center gap-2 bg-header px-4 text-on-brand sm:h-14 sm:gap-3 sm:px-6">
      <img src={ilmWhite} alt="ILM" className="block h-[22px]" />
      <span className="border-l border-white/40 pl-3 text-sm font-medium tracking-[0.01em] sm:text-[15px]">{product}</span>
      <div className="ml-auto flex min-w-0 items-center gap-2 sm:gap-3">
        <ThemeToggle placement="bar" />
        {withUser && <UserArea />}
      </div>
    </header>
  );
}

/** The frame of a page: top bar or brand-tinted background, the skip link and the main landmark. */
export function Layout({ variant, children }: Readonly<LayoutProps>) {
  if (variant === 'auth') {
    return (
      <div className="flex min-h-screen flex-col bg-surface text-content">
        <SkipLink />
        <div className="absolute right-3 top-3">
          <ThemeToggle placement="page" />
        </div>
        <main
          id="main"
          tabIndex={-1}
          className="grid focus:outline-none flex-1 place-items-center bg-[radial-gradient(1200px_400px_at_50%_-10%,color-mix(in_srgb,var(--brand)_18%,transparent),transparent_70%)] px-4 py-8"
        >
          {children}
        </main>
        <footer className="p-4 text-center text-xs text-content-subtle">
          ILM Appliance ·{' '}
          <img src={otColor} alt="OmniTrust" className="ml-1 inline h-3.5 align-[-2px] dark:hidden" />
          <img src={otWhite} alt="OmniTrust" className="ml-1 hidden h-3.5 align-[-2px] dark:inline" />
        </footer>
      </div>
    );
  }
  return (
    <div className="flex min-h-screen flex-col bg-surface text-content">
      <SkipLink />
      <TopBar product={variant === 'setup' ? 'Appliance setup' : 'Appliance'} withUser={variant === 'console'} />
      {variant === 'console' && <ConsoleNav />}
      <main id="main" tabIndex={-1} className="mx-auto focus:outline-none w-full flex-1 px-4 py-8">
        {children}
      </main>
    </div>
  );
}
