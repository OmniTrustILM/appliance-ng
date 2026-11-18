import { useCallback, useEffect, useState } from 'react';
import { applyTheme, nextThemeMode, readThemeMode, storeThemeMode, systemDarkQuery, type ThemeMode } from './theme';

export function useTheme(): { mode: ThemeMode; cycle: () => void } {
  const [mode, setMode] = useState<ThemeMode>(readThemeMode);

  useEffect(() => {
    applyTheme(mode);
    storeThemeMode(mode);
    if (mode !== 'system') return;
    const query = systemDarkQuery();
    const onChange = () => applyTheme('system');
    query.addEventListener('change', onChange);
    return () => query.removeEventListener('change', onChange);
  }, [mode]);

  const cycle = useCallback(() => setMode(nextThemeMode), []);
  return { mode, cycle };
}
