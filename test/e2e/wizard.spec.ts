// The onboarding wizard in a browser. run.sh runs each part on its own, when the appliance reaches it:
// "wizard part 1" while the first-boot service waits for the setup, "wizard part 2" once the manager took over.
import { test as base, expect } from '@playwright/test';
import { fillSecret, input, signIn, test, totpCode, writeSeed } from './appliance';

base.describe('first-boot service', () => {
  // The first-boot service presents a self-signed certificate for the node address, so only this context accepts
  // any certificate.
  base.use({ ignoreHTTPSErrors: true, baseURL: input('E2E_CONSOLE_URL') });

  base('wizard part 1', async ({ page }) => {
    const continueButton = page.getByRole('button', { name: 'Continue' });
    await page.goto('/appliance/setup/');
    await fillSecret(page.getByLabel('Setup code'), input('E2E_CODE'), 'the setup code');
    await continueButton.click();

    await expect(page.getByRole('heading', { name: 'What do you want to set up?' })).toBeVisible();
    await page.getByRole('radio', { name: /New appliance/ }).check();
    await continueButton.click();

    await expect(page.getByRole('heading', { name: 'Administrator' })).toBeVisible();
    await page.getByLabel('User name').fill(input('E2E_USER'));
    await fillSecret(page.getByLabel('Password', { exact: true }), input('E2E_PASSWORD'), 'the password');
    await fillSecret(page.getByLabel('Confirm password'), input('E2E_PASSWORD'), 'the password confirmation');
    await continueButton.click();

    await expect(page.getByRole('heading', { name: 'Authenticator' })).toBeVisible();
    // The secret is the value of its read-only field; the QR code holds the same.
    const seed = (await page.getByLabel('Secret', { exact: true }).inputValue()).trim();
    if (!seed) throw new Error('the setup page shows no TOTP secret');
    const used = new Date();
    await fillSecret(page.getByLabel('Code from the authenticator'), totpCode(seed, used), 'the TOTP code');
    await continueButton.click();
    await expect(page.getByRole('heading', { name: 'Node settings' })).toBeVisible();
    writeSeed(input('E2E_SEED_FILE'), seed, used);

    // QEMU's user network offers no router or DNS server by DHCP: the address DHCP gave becomes static with the
    // gateway and DNS server of the user network.
    await page.getByLabel('Host name').fill(input('E2E_HOST_NAME'));
    await page.getByLabel('Use a static address').check();
    await expect(page.getByLabel('Address (CIDR)')).not.toHaveValue('');
    await page.getByLabel('Gateway').fill(input('E2E_GATEWAY'));
    await page.getByLabel('DNS servers').fill(input('E2E_DNS'));
    await continueButton.click();

    await expect(page.getByRole('heading', { name: 'Review' })).toBeVisible();
    await expect(page.getByText(input('E2E_HOST_NAME'))).toBeVisible();
    await page.getByRole('button', { name: 'Apply' }).click();
    await expect(page.getByRole('heading', { name: 'Installing' })).toBeVisible();
  });
});

test('wizard part 2', async ({ console: page }) => {
  const continueButton = page.getByRole('button', { name: 'Continue' });
  const component = input('E2E_COMPONENT');
  await signIn(page);

  await expect(page.getByRole('heading', { name: 'ILM host name' })).toBeVisible();
  await page.getByLabel('ILM host name').fill(input('E2E_HOST_NAME'));
  await continueButton.click();

  await expect(page.getByRole('heading', { name: 'Certificate' })).toBeVisible();
  await continueButton.click();

  await expect(page.getByRole('heading', { name: 'Components' })).toBeVisible();
  await page.getByLabel(component).check();
  await continueButton.click();

  await expect(page.getByRole('heading', { name: 'Dependencies' })).toBeVisible();
  await continueButton.click();

  await expect(page.getByRole('heading', { name: 'Mail' })).toBeVisible();
  await expect(page.getByLabel('Send mail')).not.toBeChecked();
  await continueButton.click();

  await expect(page.getByRole('heading', { name: 'Review' })).toBeVisible();
  await expect(page.getByText(component)).toBeVisible();
  await page.getByRole('button', { name: 'Install' }).click();
  await expect(page.getByRole('heading', { name: 'Installing the platform' })).toBeVisible();
});
