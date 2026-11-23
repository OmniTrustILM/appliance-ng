const byteUnits = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

/** Formats a size in binary multiples with one decimal, such as 60.0 GB. */
export function formatBytes(bytes: number) {
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < byteUnits.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${byteUnits[unit]}`;
}
