// Shared steps of the browser tests: the inputs run.sh passes, TOTP codes, trust in the console's certificate and
// sign-in.
// Nothing here prints the setup code, the password, the TOTP seed or a TOTP code: the boot test log is public.
import { createHash, createHmac, X509Certificate } from 'node:crypto';
import { chmodSync, readFileSync, statSync, utimesSync, writeFileSync } from 'node:fs';
import { connect } from 'node:tls';
import { setTimeout as sleep } from 'node:timers/promises';
import { test as base, chromium, expect, request, type Locator, type Page } from '@playwright/test';

/** Reads a required input from the environment. */
export function input(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is not set`);
  return value;
}

const TOTP_PERIOD = 30;
const BASE32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
/** Sign-in attempts: the appliance blocks a user at an address after 5 failures, a block that would stop the tests. */
const SIGN_IN_ATTEMPTS = 4;
const POLL = 5_000;
/** How long the built-in CA may take to issue the console's certificate after the hand-over. */
const CERTIFICATE_TIMEOUT = 15 * 60_000;

function base32(text: string): Buffer {
  const bytes: number[] = [];
  let value = 0;
  let bits = 0;
  let end = text.length;
  while (end > 0 && text[end - 1] === '=') end--;
  for (const char of text.slice(0, end).toUpperCase()) {
    const index = BASE32.indexOf(char);
    if (index < 0) throw new Error('the TOTP seed is not valid base32');
    value = ((value << 5) | index) & 0xffff;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      bytes.push((value >>> bits) & 0xff);
    }
  }
  return Buffer.from(bytes);
}

/** The TOTP code of seed at time as the appliance computes it: SHA-1, 6 digits, 30-second steps. */
export function totpCode(seed: string, time: Date): string {
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(time.getTime() / 1000 / TOTP_PERIOD)));
  const mac = createHmac('sha1', base32(seed)).update(counter).digest();
  const offset = mac[mac.length - 1] & 0x0f;
  return ((mac.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).toString().padStart(6, '0');
}

/**
 * Writes the seed to path, readable by the owner only. The file's modification time records the last TOTP step
 * used, which test/boot/drive reads as well: the appliance refuses a code of a step used before.
 */
export function writeSeed(path: string, seed: string, used: Date) {
  writeFileSync(path, seed, { mode: 0o600 });
  chmodSync(path, 0o600);
  utimesSync(path, used, used);
}

/**
 * Fills field with a secret. Playwright adds a failed action's call log, which holds the value, to its error, so a
 * failure is replaced by an error that names only what.
 */
export async function fillSecret(field: Locator, value: string, what: string) {
  try {
    await field.fill(value);
  } catch {
    throw new Error(`could not fill ${what}`);
  }
}

/** A TOTP code of a step later than the last one used, which it records. It waits for that step to begin. */
async function freshCode(path: string): Promise<string> {
  const seed = readFileSync(path, 'utf8').trim();
  const step = (ms: number) => Math.floor(ms / 1000 / TOTP_PERIOD);
  const used = step(statSync(path).mtime.getTime());
  // A timer may fire a little early, so the step is checked again after it.
  while (step(Date.now()) <= used) await sleep((used + 1) * TOTP_PERIOD * 1000 - Date.now() + 10);
  const now = new Date();
  utimesSync(path, now, now);
  return totpCode(seed, now);
}

/** The console reached by the appliance's host name, as run.sh forwards it. */
function consoleOrigin() {
  const { hostname: address, port } = new URL(input('E2E_CONSOLE_URL'));
  const host = input('E2E_HOST_NAME');
  return { address, port: Number(port), host, url: `https://${host}:${port}` };
}

/**
 * Downloads the root CA from the console. It is not verified here: the root CA is trusted on first use and must then
 * pass servedCertificate.
 */
async function fetchRootCA(): Promise<string> {
  const context = await request.newContext({ ignoreHTTPSErrors: true });
  try {
    const res = await context.get(`${input('E2E_CONSOLE_URL')}/appliance/api/v1/ca.pem`, { timeout: 10_000 });
    if (!res.ok()) throw new Error(`GET /appliance/api/v1/ca.pem: HTTP ${res.status()}`);
    return await res.text();
  } finally {
    await context.dispose();
  }
}

/**
 * Connects to the console for host and returns the certificate it presents, verified against root when one is given
 * and else unverified.
 */
function servedCertificate(address: string, port: number, host: string, root?: string): Promise<X509Certificate> {
  const trust = root ? { ca: root } : { rejectUnauthorized: false };
  return new Promise((resolve, reject) => {
    const socket = connect({ host: address, port, servername: host, ...trust, timeout: 10_000 }, () => {
      const leaf = socket.getPeerX509Certificate();
      socket.end();
      if (leaf) resolve(leaf);
      else reject(new Error('the console presented no certificate'));
    });
    socket.on('timeout', () => socket.destroy(new Error('the TLS handshake timed out')));
    socket.on('error', reject);
  });
}

/** The base64 SHA-256 of a certificate's public key, as Chromium's certificate error allow-list takes it. */
function spki(certificate: X509Certificate): string {
  return createHash('sha256').update(certificate.publicKey.export({ type: 'spki', format: 'der' })).digest('base64');
}

/** The setup certificate of the first-boot service that run.sh saved, if it passed one. */
function setupCertificate(): X509Certificate | undefined {
  const path = process.env.E2E_SETUP_CERT_FILE;
  return path ? new X509Certificate(readFileSync(path)) : undefined;
}

/**
 * Waits until the console serves the setup certificate, which it does until setup is complete, or a certificate of
 * the built-in CA for its host name, as test/boot/drive does: the root CA comes from the console itself (trusted on
 * first use) and must have issued the certificate the console presented, for the host name. Returns the public key
 * hashes Chromium trusts: the setup certificate's, or the root's and the verified leaf's for a console that does not
 * send the root in its chain.
 */
async function trustConsole(): Promise<string[]> {
  const { address, port, host } = consoleOrigin();
  const setup = setupCertificate();
  const deadline = Date.now() + CERTIFICATE_TIMEOUT;
  let root = '';
  for (;;) {
    try {
      if (setup && (await servedCertificate(address, port, host)).fingerprint256 === setup.fingerprint256) {
        console.log('the console serves the setup certificate');
        return [spki(setup)];
      }
      root ||= await fetchRootCA();
      const leaf = await servedCertificate(address, port, host, root);
      console.log(`console certificate for ${host} issued by ${leaf.issuer.replaceAll('\n', ', ')}`);
      return [spki(new X509Certificate(root)), spki(leaf)];
    } catch (error) {
      if (Date.now() > deadline) throw error;
    }
    await sleep(POLL);
  }
}

/**
 * A test with `console`: a page of a Chromium that reaches the console by the appliance's host name and trusts
 * only the certificates trustConsole checked.
 */
export const test = base.extend<{ console: Page }>({
  console: async ({}, use) => {
    const { address, host, url } = consoleOrigin();
    const pins = await trustConsole();
    const browser = await chromium.launch({
      args: [`--host-resolver-rules=MAP ${host} ${address}`, `--ignore-certificate-errors-spki-list=${pins.join(',')}`],
    });
    try {
      const context = await browser.newContext({ baseURL: url });
      await use(await context.newPage());
    } finally {
      await browser.close();
    }
  },
});

/**
 * Signs in with the password and a fresh TOTP code. Failures are retried a few times, each with a new step, as the
 * administrator may not be imported yet; a lock-out ends the test at once.
 */
export async function signIn(page: Page) {
  const signInButton = page.getByRole('button', { name: 'Sign in' });
  const signedIn = page.getByRole('button', { name: 'Sign out' });
  const lockedOut = page.getByText('Too many failed attempts.');
  await page.goto('/appliance/signin');
  for (let attempt = 1; ; attempt++) {
    await page.getByLabel('User name').fill(input('E2E_USER'));
    await fillSecret(page.getByLabel('Password'), input('E2E_PASSWORD'), 'the password');
    await fillSecret(page.getByLabel('Authenticator code'), await freshCode(input('E2E_SEED_FILE')), 'the TOTP code');
    await signInButton.click();
    try {
      await expect(signedIn.or(lockedOut)).toBeVisible({ timeout: 15_000 });
    } catch {
      if (attempt === SIGN_IN_ATTEMPTS) throw new Error(`sign-in failed ${attempt} times`);
      console.log(`sign-in attempt ${attempt} failed, retrying`);
      continue;
    }
    if (await lockedOut.isVisible()) throw new Error('sign-in is locked: too many failed attempts');
    return;
  }
}
