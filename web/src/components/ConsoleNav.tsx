import { NavLink } from 'react-router';
import { cx } from './cx';

/** The console's pages in menu order; adding a page to the menu is one more entry here. */
export const consoleLinks = [
  { to: '/', label: 'Dashboard' },
  { to: '/node', label: 'Node' },
  { to: '/certificate', label: 'Certificate' },
  { to: '/trust', label: 'Trusted CAs' },
  { to: '/registries', label: 'Registries' },
  { to: '/platform', label: 'Components' },
  { to: '/mail', label: 'Mail' },
  { to: '/access', label: 'Cluster access' },
  { to: '/recovery-kit', label: 'Recovery kit' },
  { to: '/backups', label: 'Backups' },
  { to: '/system', label: 'System' },
  { to: '/audit', label: 'Audit log' },
] as const;

/** The console menu: a row under the top bar that scrolls sideways on a narrow screen. */
export function ConsoleNav() {
  return (
    <nav aria-label="Console" className="border-b border-divider bg-surface-raised">
      <ul className="m-0 flex list-none overflow-x-auto px-2 sm:px-4">
        {consoleLinks.map(({ to, label }) => (
          <li key={to} className="flex-none">
            <NavLink
              to={to}
              end
              className={({ isActive }) =>
                cx(
                  'block whitespace-nowrap border-b-2 px-3 py-3 text-sm font-medium no-underline',
                  isActive ? 'border-brand text-brand' : 'border-transparent text-content-muted hover:text-content',
                )
              }
            >
              {label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}
