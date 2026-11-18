import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ThemeToggle from './ThemeToggle';
import { applyTheme, nextThemeMode, readThemeMode, storeThemeMode } from './theme';

let matches = false;
let listeners: Array<() => void> = [];

beforeEach(() => {
  matches = false;
  listeners = [];
  window.localStorage.clear();
  document.documentElement.classList.remove('dark');
  vi.stubGlobal(
    'matchMedia',
    vi.fn(() => ({
      get matches() {
        return matches;
      },
      addEventListener: (_: string, fn: () => void) => listeners.push(fn),
      removeEventListener: (_: string, fn: () => void) => {
        listeners = listeners.filter((l) => l !== fn);
      },
    })),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('theme mode', () => {
  it('defaults to system', () => {
    expect(readThemeMode()).toBe('system');
  });

  it('cycles system, light, dark', () => {
    expect(nextThemeMode('system')).toBe('light');
    expect(nextThemeMode('light')).toBe('dark');
    expect(nextThemeMode('dark')).toBe('system');
  });

  it('stores the choice and forgets system', () => {
    storeThemeMode('dark');
    expect(window.localStorage.getItem('theme-mode')).toBe('dark');
    expect(readThemeMode()).toBe('dark');
    storeThemeMode('system');
    expect(window.localStorage.getItem('theme-mode')).toBeNull();
  });

  it('falls back to system when storage throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(readThemeMode()).toBe('system');
    expect(() => storeThemeMode('dark')).not.toThrow();
  });

  it('applies dark and light', () => {
    applyTheme('dark');
    expect(document.documentElement).toHaveClass('dark');
    matches = true;
    applyTheme('light');
    expect(document.documentElement).not.toHaveClass('dark');
  });
});

describe('ThemeToggle', () => {
  it('follows a system change while in system mode', () => {
    render(<ThemeToggle placement="bar" />);
    expect(document.documentElement).not.toHaveClass('dark');
    matches = true;
    act(() => listeners.forEach((fn) => fn()));
    expect(document.documentElement).toHaveClass('dark');
  });

  it('cycles and labels', () => {
    render(<ThemeToggle placement="page" />);
    const button = screen.getByRole('button', { name: 'Theme: System. Switch to Light.' });
    fireEvent.click(button);
    expect(screen.getByRole('button', { name: 'Theme: Light. Switch to Dark.' })).toHaveAttribute(
      'title',
      'Theme: Light',
    );
    expect(window.localStorage.getItem('theme-mode')).toBe('light');
  });

  it('draws an icon in every mode', () => {
    const { container } = render(<ThemeToggle placement="page" />);
    const labels = ['Theme: System. Switch to Light.', 'Theme: Light. Switch to Dark.', 'Theme: Dark. Switch to System.'];
    for (const label of labels) {
      const button = screen.getByRole('button', { name: label });
      expect(container.querySelector('svg')).toBeInTheDocument();
      fireEvent.click(button);
    }
  });
});
