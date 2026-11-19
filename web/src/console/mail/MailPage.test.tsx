import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { MailPage } from './MailPage';

const stored = { host: 'smtp.example.test', port: 587, security: 'starttls', from: 'ilm@example.test', username: 'mailer', passwordSet: true };
const withMail = [200, { mail: stored }] as const;

afterEach(() => vi.unstubAllGlobals());

describe('MailPage', () => {
  it('prefills the saved settings, keeps the password and sends no password when it is left empty', async () => {
    const { sent } = stubApi({ 'GET /api/v1/mail': withMail, 'PUT /api/v1/mail': [202] });
    render(<MailPage />);
    expect(await screen.findByLabelText('Mail server')).toHaveValue('smtp.example.test');
    expect(screen.getByLabelText('Port')).toHaveValue('587');
    expect(screen.getByLabelText('Security')).toHaveValue('starttls');
    expect(screen.getByLabelText('Password (optional)')).toHaveAccessibleDescription(/leave the field empty to keep it/i);
    await userEvent.clear(screen.getByLabelText('From address'));
    await userEvent.type(screen.getByLabelText('From address'), 'new@example.test');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/change is saved/i)).toBeInTheDocument();
    expect(sent['PUT /api/v1/mail']).toEqual([
      { host: 'smtp.example.test', port: 587, security: 'starttls', from: 'new@example.test', username: 'mailer' },
    ]);
  });

  it('stops offering to keep the password once the server changes', async () => {
    stubApi({ 'GET /api/v1/mail': withMail });
    render(<MailPage />);
    await userEvent.type(await screen.findByLabelText('Mail server'), '2');
    expect(screen.getByLabelText('Password (optional)')).not.toHaveAccessibleDescription(/keep it/i);
  });

  it('sets a new mail server with a password when none is saved', async () => {
    const { sent } = stubApi({ 'GET /api/v1/mail': [200, {}], 'PUT /api/v1/mail': [202] });
    render(<MailPage />);
    expect(await screen.findByLabelText('Port')).toHaveValue('25');
    expect(screen.queryByRole('button', { name: 'Remove the mail server' })).not.toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Mail server'), 'smtp.example.test');
    await userEvent.type(screen.getByLabelText('From address'), 'ilm@example.test');
    await userEvent.selectOptions(screen.getByLabelText('Security'), 'tls');
    await userEvent.type(screen.getByLabelText('User name (optional)'), 'mailer');
    await userEvent.type(screen.getByLabelText('Password (optional)'), 'mail secret');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText(/change is saved/i);
    expect(sent['PUT /api/v1/mail']).toEqual([
      { host: 'smtp.example.test', port: 25, security: 'tls', from: 'ilm@example.test', username: 'mailer', password: 'mail secret' },
    ]);
  });

  it('checks the fields before saving, including credentials without TLS', async () => {
    const { sent } = stubApi({ 'GET /api/v1/mail': [200, {}] });
    render(<MailPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Save' }));
    expect(screen.getByLabelText('Mail server')).toHaveAccessibleDescription(/required/i);
    expect(sent['PUT /api/v1/mail']).toBeUndefined();
    await userEvent.type(screen.getByLabelText('Mail server'), 'smtp.example.test');
    await userEvent.type(screen.getByLabelText('From address'), 'ilm@example.test');
    await userEvent.type(screen.getByLabelText('User name (optional)'), 'mailer');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByLabelText('User name (optional)')).toHaveAccessibleDescription(/starttls or tls/);
    expect(sent['PUT /api/v1/mail']).toBeUndefined();
  });

  it.each([
    ['username', 'User name (optional)', 'Credentials need security starttls or tls; choose one, or remove the user name.'],
    ['password', 'Password (optional)', 'Enter the password for this user.'],
  ])('shows a refused %s at its field', async (field, label, message) => {
    stubApi({ 'GET /api/v1/mail': withMail, 'PUT /api/v1/mail': [400, { code: 'invalid', field, message }] });
    render(<MailPage />);
    await userEvent.type(await screen.findByLabelText('Mail server'), '2');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByLabelText(label)).toHaveAccessibleDescription(message);
  });

  it('shows another refusal and an unreachable appliance as alerts', async () => {
    const { mock } = stubApi({ 'GET /api/v1/mail': withMail, 'PUT /api/v1/mail': [400, { code: 'invalid', message: 'Refused.' }] });
    render(<MailPage />);
    await screen.findByLabelText('Mail server');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Refused.');
    mock.mockRejectedValueOnce(new TypeError('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
  });

  it('removes the mail server after a confirmation', async () => {
    let mail: object = stored;
    const { sent } = stubApi({
      'GET /api/v1/mail': () => [200, { mail }],
      'DELETE /api/v1/mail': () => { mail = undefined as never; return [202]; },
    });
    render(<MailPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Remove the mail server' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }));
    expect(sent['DELETE /api/v1/mail']).toBeUndefined();
    await userEvent.click(screen.getByRole('button', { name: 'Remove the mail server' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText('The mail server is removed.')).toBeInTheDocument();
    expect(await screen.findByLabelText('Mail server')).toHaveValue('');
  });

  it('explains which component needs the mail server when it cannot be removed', async () => {
    stubApi({
      'GET /api/v1/mail': withMail,
      'DELETE /api/v1/mail': [409, { code: 'mail_in_use', message: 'The email sends mail; remove it from the components first.' }],
    });
    render(<MailPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Remove the mail server' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('The email sends mail');
    expect(screen.getByLabelText('Mail server')).toHaveValue('smtp.example.test');
  });

  it('reports settings that cannot be loaded', async () => {
    stubApi({ 'GET /api/v1/mail': [409, { code: 'conflict', message: 'Setup is not done.' }] });
    render(<MailPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup is not done.');
  });

  describe('test mail', () => {
    const send = async () => {
      await userEvent.type(await screen.findByLabelText('Send the test mail to'), 'me@example.test');
      await userEvent.click(screen.getByRole('button', { name: 'Send a test mail' }));
    };

    it('sends to the recipient and reports success', async () => {
      const { sent } = stubApi({ 'GET /api/v1/mail': withMail, 'POST /api/v1/mail/test': [204] });
      render(<MailPage />);
      await send();
      expect(await screen.findByText('The test mail was sent to me@example.test.')).toBeInTheDocument();
      expect(sent['POST /api/v1/mail/test']).toEqual([{ to: 'me@example.test' }]);
    });

    it('drops the result once the settings change', async () => {
      stubApi({ 'GET /api/v1/mail': withMail, 'POST /api/v1/mail/test': [204] });
      render(<MailPage />);
      await send();
      expect(await screen.findByText('The test mail was sent to me@example.test.')).toBeInTheDocument();
      await userEvent.type(screen.getByLabelText('Mail server'), '2');
      expect(screen.queryByText('The test mail was sent to me@example.test.')).not.toBeInTheDocument();
    });

    it.each([
      ['connect', /cannot be reached/],
      ['tls', /TLS failed/],
      ['auth', /refused the user name or password/],
      ['send', /refused the message/],
    ])('says what failed at the %s stage', async (stage, text) => {
      stubApi({
        'GET /api/v1/mail': withMail,
        'POST /api/v1/mail/test': [502, { code: 'mail_failed', message: 'x', stage }],
      });
      render(<MailPage />);
      await send();
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
    });

    it('shows other refusals and an unreachable appliance', async () => {
      const { mock } = stubApi({
        'GET /api/v1/mail': withMail,
        'POST /api/v1/mail/test': [400, { code: 'invalid', message: 'Enter a valid address.' }],
      });
      render(<MailPage />);
      await send();
      expect(await screen.findByRole('alert')).toHaveTextContent('Enter a valid address.');
      mock.mockRejectedValueOnce(new TypeError('offline'));
      await userEvent.click(screen.getByRole('button', { name: 'Send a test mail' }));
      expect(await screen.findByRole('alert')).toHaveTextContent('cannot be reached');
    });

    it('is disabled while the form has unsaved changes or no mail server is saved', async () => {
      stubApi({ 'GET /api/v1/mail': withMail });
      const first = render(<MailPage />);
      await userEvent.type(await screen.findByLabelText('Send the test mail to'), 'me@example.test');
      expect(screen.getByRole('button', { name: 'Send a test mail' })).toBeEnabled();
      await userEvent.type(screen.getByLabelText('Mail server'), '2');
      expect(screen.getByRole('button', { name: 'Send a test mail' })).toBeDisabled();
      expect(screen.getByText(/uses the saved mail settings/i)).toBeInTheDocument();
      first.unmount();
      stubApi({ 'GET /api/v1/mail': [200, {}] });
      render(<MailPage />);
      await userEvent.type(await screen.findByLabelText('Send the test mail to'), 'me@example.test');
      expect(screen.getByRole('button', { name: 'Send a test mail' })).toBeDisabled();
    });
  });
});
