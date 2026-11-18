export type ThemeMode = 'system' | 'light' | 'dark';

const STORAGE_KEY = 'theme-mode';
const DARK_QUERY = '(prefers-color-scheme: dark)';
const NEXT: Record<ThemeMode, ThemeMode> = { system: 'light', light: 'dark', dark: 'system' };

export const THEME_LABELS: Record<ThemeMode, string> = { system: 'System', light: 'Light', dark: 'Dark' };

export function readThemeMode(): ThemeMode {
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    return stored === 'light' || stored === 'dark' ? stored : 'system';
  } catch {
    return 'system';
  }
}

export function storeThemeMode(mode: ThemeMode): void {
  try {
    if (mode === 'system') {
      window.localStorage.removeItem(STORAGE_KEY);
    } else {
      window.localStorage.setItem(STORAGE_KEY, mode);
    }
  } catch {
    // Storage can be unavailable (private mode, blocked); the choice then lasts for the session only.
  }
}

export function nextThemeMode(mode: ThemeMode): ThemeMode {
  return NEXT[mode];
}

export function systemDarkQuery(): MediaQueryList {
  return window.matchMedia(DARK_QUERY);
}

export function applyTheme(mode: ThemeMode): void {
  const dark = mode === 'dark' || (mode === 'system' && systemDarkQuery().matches);
  document.documentElement.classList.toggle('dark', dark);
}
