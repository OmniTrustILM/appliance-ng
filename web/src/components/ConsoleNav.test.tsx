import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';
import { ConsoleNav, consoleLinks } from './ConsoleNav';

function renderAt(path: string) {
  render(<MemoryRouter initialEntries={[path]}><ConsoleNav /></MemoryRouter>);
}

describe('ConsoleNav', () => {
  it('lists the console pages as links', () => {
    renderAt('/');
    const links = screen.getAllByRole('link');
    expect(links.map((link) => link.textContent)).toEqual(consoleLinks.map((entry) => entry.label));
    expect(screen.getByRole('link', { name: 'Node' })).toHaveAttribute('href', '/node');
    expect(screen.getByRole('link', { name: 'Certificate' })).toHaveAttribute('href', '/certificate');
    expect(screen.getByRole('link', { name: 'Trusted CAs' })).toHaveAttribute('href', '/trust');
    expect(screen.getByRole('link', { name: 'Registries' })).toHaveAttribute('href', '/registries');
    expect(screen.getByRole('link', { name: 'Components' })).toHaveAttribute('href', '/platform');
    expect(screen.getByRole('link', { name: 'Mail' })).toHaveAttribute('href', '/mail');
    expect(screen.getByRole('link', { name: 'Audit log' })).toHaveAttribute('href', '/audit');
  });

  it('marks only the active page as current', () => {
    renderAt('/node');
    expect(screen.getByRole('link', { name: 'Node' })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'Dashboard' })).not.toHaveAttribute('aria-current');
    expect(screen.getByRole('link', { name: 'System' })).not.toHaveAttribute('aria-current');
  });

  it('marks the dashboard on the start page', () => {
    renderAt('/');
    expect(screen.getByRole('link', { name: 'Dashboard' })).toHaveAttribute('aria-current', 'page');
  });
});
