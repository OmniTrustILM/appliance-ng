const label = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/i;
const userName = /^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$/;

/** Shows a setup code as the appliance prints it: upper case, in two groups of five. */
export function normaliseCode(input: string): string {
  const characters = input.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 10);
  return characters.length > 5 ? `${characters.slice(0, 5)}-${characters.slice(5)}` : characters;
}

/** Splits a list typed with commas, spaces or new lines. */
export function splitList(input: string): string[] {
  return input.split(/[\s,]+/).filter(Boolean);
}

/** A fully qualified host name, not an IP address. */
export function isFqdn(value: string): boolean {
  const labels = value.split('.');
  return value.length <= 253 && labels.length >= 2 && labels.every((part) => label.test(part)) && !isIpAddress(value);
}

export function isIpv4(value: string): boolean {
  const parts = value.split('.');
  return parts.length === 4 && parts.every((part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255);
}

/** An IPv4 address with a prefix length of 8 to 30. */
export function isIpv4Cidr(value: string): boolean {
  const [address, prefix, ...rest] = value.split('/');
  return rest.length === 0 && isIpv4(address) && /^\d{1,2}$/.test(prefix ?? '') && Number(prefix) >= 8 && Number(prefix) <= 30;
}

export function isIpAddress(value: string): boolean {
  return isIpv4(value) || (value.includes(':') && /^[0-9a-f:.]+$/i.test(value));
}

export function isHostOrAddress(value: string): boolean {
  return isIpAddress(value) || isFqdn(value) || label.test(value);
}

export function isProxyUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.hostname !== '';
  } catch {
    return false;
  }
}

export type AdminInput = { user: string; password: string; confirmation: string };

/** The problems the server would report for the administrator, by field. */
export function adminProblems({ user, password, confirmation }: AdminInput) {
  const problems: Partial<Record<'user' | 'password' | 'confirmation', string>> = {};
  if (!userName.test(user)) {
    problems.user = 'Use letters, digits and . _ @ - , starting with a letter or digit (at most 64 characters).';
  }
  if (password.length < 12) {
    problems.password = 'Use at least 12 characters.';
  } else if (user && password.toLowerCase().includes(user.toLowerCase())) {
    problems.password = 'The password must not contain the user name.';
  }
  if (confirmation !== password) {
    problems.confirmation = 'The confirmation does not match the password.';
  }
  return problems;
}
