import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { RecoveryKitPage } from './RecoveryKitPage';

const notIssued = { issued: false, confirmed: false };
const issued = {
  issued: true, confirmed: false, issuedAt: '2026-11-21T10:00:00Z', talosconfigNotAfter: '2027-11-21T10:00:00Z',
  recipient: 'age1recipient',
};
const confirmed = { ...issued, confirmed: true };
const kitFile = 'age-encryption.org/v1\n';
const passphrase = 'a long passphrase';
const click = vi.fn();

beforeEach(() => {
  URL.createObjectURL = vi.fn(() => 'blob:kit');
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    click(this.download, this.href);
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  click.mockReset();
});

function renderPage() {
  return render(<MemoryRouter><RecoveryKitPage /></MemoryRouter>);
}

async function typePassphrases(label: string, value = passphrase, repeat = value) {
  await screen.findByRole('form', { name: 'Issue the recovery kit' });
  await userEvent.type(screen.getByLabelText(label), value);
  await userEvent.type(screen.getByLabelText(`Repeat the ${label.toLowerCase()}`), repeat);
}

describe('RecoveryKitPage', () => {
  it('issues the kit, downloads it without keeping it and asks to confirm it', async () => {
    let state: object = notIssued;
    const { sent } = stubApi({
      'GET /appliance/api/v1/recovery-kit': () => [200, state],
      'POST /appliance/api/v1/recovery-kit': () => {
        state = issued;
        return [200, kitFile];
      },
      'POST /appliance/api/v1/recovery-kit/confirm': () => {
        state = confirmed;
        return [204];
      },
    });
    renderPage();
    await typePassphrases('Passphrase');
    await userEvent.click(await screen.findByRole('button', { name: 'Issue the recovery kit' }));
    expect(sent['POST /appliance/api/v1/recovery-kit']).toEqual([{ passphrase }]);
    expect(click).toHaveBeenCalledWith('ilm-appliance-recovery-kit.tar.age', 'blob:kit');
    expect(await screen.findByText(/not confirmed/i)).toBeInTheDocument();
    expect(screen.queryByText(kitFile.trim())).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'I have stored the kit safely' }));
    expect(sent['POST /appliance/api/v1/recovery-kit/confirm']).toHaveLength(1);
    expect(await screen.findByText(/the kit is confirmed/i)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'I have stored the kit safely' })).not.toBeInTheDocument();
  });

  it('does not issue a kit while the passphrases differ and shows a refused passphrase at its field', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/recovery-kit': [200, notIssued],
      'POST /appliance/api/v1/recovery-kit': [400, { code: 'invalid', message: 'The password is too common.', field: 'passphrase' }],
    });
    renderPage();
    await typePassphrases('Passphrase', passphrase, 'another passphrase');
    const submit = await screen.findByRole('button', { name: 'Issue the recovery kit' });
    expect(submit).toBeDisabled();
    expect(screen.getByText('The passphrases differ.')).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText('Repeat the passphrase'));
    await userEvent.type(screen.getByLabelText('Repeat the passphrase'), passphrase);
    await userEvent.click(submit);
    expect(await screen.findByText('The password is too common.')).toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/recovery-kit']).toHaveLength(1);
    expect(click).not.toHaveBeenCalled();
  });

  it('re-issues the kit from the current one with a new passphrase', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/recovery-kit': [200, confirmed],
      'POST /appliance/api/v1/recovery-kit/reissue': [200, kitFile],
    });
    renderPage();
    expect(await screen.findByText(/the kit is confirmed/i)).toBeInTheDocument();
    const form = screen.getByRole('form', { name: 'Issue the kit again' });
    await userEvent.upload(within(form).getByLabelText(/current kit/i), new File(['kit'], 'kit.tar.age'));
    await userEvent.type(within(form).getByLabelText('Current passphrase'), 'the old passphrase');
    await userEvent.type(within(form).getByLabelText('New passphrase'), passphrase);
    await userEvent.type(within(form).getByLabelText('Repeat the new passphrase'), passphrase);
    await userEvent.click(within(form).getByRole('button', { name: 'Issue the kit again' }));
    expect(await screen.findByText(/was downloaded/)).toBeInTheDocument();
    const [body] = sent['POST /appliance/api/v1/recovery-kit/reissue'] as Record<string, unknown>[];
    expect(body.passphrase).toBe('the old passphrase');
    expect(body.newPassphrase).toBe(passphrase);
    expect(body.kit).toBeInstanceOf(File);
    expect(click).toHaveBeenCalledWith('ilm-appliance-recovery-kit.tar.age', 'blob:kit');
    expect(within(form).getByLabelText('Current passphrase')).toHaveValue('');
    expect(within(form).getByLabelText('New passphrase')).toHaveValue('');
    expect(within(form).getByLabelText('Repeat the new passphrase')).toHaveValue('');
    expect(within(form).getByRole('button', { name: 'Issue the kit again' })).toBeDisabled();
  });

  it('shows why a re-issue was refused', async () => {
    stubApi({
      'GET /appliance/api/v1/recovery-kit': [200, confirmed],
      'POST /appliance/api/v1/recovery-kit/reissue': [400, { code: 'wrong_passphrase', message: 'The passphrase does not open the kit.' }],
    });
    renderPage();
    const form = await screen.findByRole('form', { name: 'Issue the kit again' });
    await userEvent.upload(within(form).getByLabelText(/current kit/i), new File(['kit'], 'kit.tar.age'));
    await userEvent.type(within(form).getByLabelText('Current passphrase'), 'wrong passphrase');
    await userEvent.type(within(form).getByLabelText('New passphrase'), passphrase);
    await userEvent.type(within(form).getByLabelText('Repeat the new passphrase'), passphrase);
    await userEvent.click(within(form).getByRole('button', { name: 'Issue the kit again' }));
    expect(await screen.findByText('The passphrase does not open the kit.')).toBeInTheDocument();
    expect(click).not.toHaveBeenCalled();
  });

  it('warns before replacing a lost kit that earlier backups stay readable only with it', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/recovery-kit': [200, confirmed],
      'POST /appliance/api/v1/recovery-kit/replace': [200, kitFile],
    });
    renderPage();
    const form = await screen.findByRole('form', { name: 'Replace a lost kit' });
    await userEvent.type(within(form).getByLabelText('New passphrase'), passphrase);
    await userEvent.type(within(form).getByLabelText('Repeat the new passphrase'), passphrase);
    await userEvent.click(within(form).getByRole('button', { name: 'Replace the kit' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent(/backups made so far stay readable only with the lost kit/i);
    expect(sent['POST /appliance/api/v1/recovery-kit/replace']).toBeUndefined();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Replace the kit' }));
    expect(await screen.findByText(/was downloaded/)).toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/recovery-kit/replace']).toEqual([{ passphrase, confirm: true }]);
    expect(within(form).getByLabelText('New passphrase')).toHaveValue('');
    expect(within(form).getByRole('button', { name: 'Replace the kit' })).toBeDisabled();
  });

  it('reports a state that cannot be read', async () => {
    stubApi({ 'GET /appliance/api/v1/recovery-kit': [500, { code: 'internal', message: 'The request failed.' }] });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('The request failed.');
  });
});
