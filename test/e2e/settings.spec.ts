// Settings of a set-up appliance in a browser: DNS servers, a trusted CA, a component and the audit log of them.
// run.sh runs it after every readiness check: the trusted CA restarts the ILM pods and the component is removed.
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { expect, type Page } from '@playwright/test';
import { input, signIn, test } from './appliance';

/** A throwaway self-signed CA certificate in PEM; its key is discarded. */
function throwawayCA(commonName: string): string {
  return execFileSync('openssl', [
    'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-keyout', '/dev/null',
    '-subj', `/CN=${commonName}`, '-days', '1',
    '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign',
  ], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
}

async function open(page: Page, link: string, heading: string) {
  await page.getByRole('navigation', { name: 'Console' }).getByRole('link', { name: link, exact: true }).click();
  await expect(page.getByRole('heading', { name: heading, exact: true })).toBeVisible();
}

test('settings', async ({ console: page }) => {
  const save = page.getByRole('button', { name: 'Save' });
  await signIn(page);
  await expect(page).toHaveURL(/\/$/);

  await test.step('DNS servers', async () => {
    await open(page, 'Node', 'Node');
    const dns = page.getByLabel('DNS servers');
    const resolver = input('E2E_DNS');
    await expect(dns).toHaveValue(resolver);
    // The QEMU resolver stays first, so name resolution keeps working.
    await dns.fill(`${resolver}, 192.0.2.53`);
    await save.click();
    await expect(page.getByText('The node settings are saved and are being applied.')).toBeVisible();
  });

  await test.step('trusted CA', async () => {
    const commonName = `Browser test CA ${randomUUID().slice(0, 8)}`;
    await open(page, 'Trusted CAs', 'Trusted CAs');
    await page.getByLabel('Or paste the certificates').fill(throwawayCA(commonName));
    await page.getByRole('button', { name: 'Preview' }).click();
    await expect(page.getByRole('heading', { name: 'New certificates (1)' })).toBeVisible();
    await page.getByRole('button', { name: 'Add the new certificates' }).click();
    await expect(page.getByText('The change is saved.')).toBeVisible();
    // Each trusted CA has a remove button named after its subject.
    await expect(page.getByRole('button', { name: commonName })).toBeVisible();
  });

  await test.step('component', async () => {
    await open(page, 'Components', 'Components');
    const component = page.getByLabel(input('E2E_COMPONENT'));
    await expect(component).toBeChecked();
    await component.uncheck();
    await save.click();
    await expect(page.getByText('The change is saved.')).toBeVisible();
    await expect(component).not.toBeChecked();
  });

  await test.step('audit log', async () => {
    await open(page, 'Audit log', 'Audit log');
    for (const action of ['sign-in', 'node-settings', 'trust-add', 'platform-settings']) {
      const entry = page.getByRole('row').filter({ hasText: action }).filter({ hasText: 'Success' });
      await expect(entry.first(), `audit entry ${action}`).toBeVisible();
    }
  });
});
