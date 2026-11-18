import { cx } from '../components/cx';
import { MonitorIcon, MoonIcon, SunIcon } from './icons';
import { nextThemeMode, THEME_LABELS } from './theme';
import { useTheme } from './useTheme';

const ICONS = { system: MonitorIcon, light: SunIcon, dark: MoonIcon };

const PLACEMENT_CLASSES = {
  bar: 'text-on-brand hover:bg-white/15 focus-visible:outline-white',
  page: 'text-content-muted hover:bg-surface-hover hover:text-content',
};

export default function ThemeToggle({ placement }: Readonly<{ placement: 'bar' | 'page' }>) {
  const { mode, cycle } = useTheme();
  const CurrentIcon = ICONS[mode];
  return (
    <button
      type="button"
      onClick={cycle}
      title={`Theme: ${THEME_LABELS[mode]}`}
      aria-label={`Theme: ${THEME_LABELS[mode]}. Switch to ${THEME_LABELS[nextThemeMode(mode)]}.`}
      className={cx('inline-flex h-9 w-9 cursor-pointer items-center justify-center rounded-full', PLACEMENT_CLASSES[placement])}
    >
      <CurrentIcon />
    </button>
  );
}
