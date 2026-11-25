import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AdminPage } from './AdminPage';
import { stubApi } from './test-utils';

afterEach(() => vi.unstubAllGlobals());

async function fill(user: string, password: string, confirmation: string) {
  await userEvent.type(screen.getByLabelText('User name'), user);
  await userEvent.type(screen.getByLabelText('Password'), password);
  await userEvent.type(screen.getByLabelText('Confirm password'), confirmation);
  await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
}

describe('AdminPage', () => {
  it('checks the fields before it sends anything', async () => {
    const { mock } = stubApi({});
    render(<AdminPage onBack={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await fill('admin', 'short', 'other');
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(/12 characters/);
    expect(screen.getByLabelText('Confirm password')).toHaveAccessibleDescription(/match/);
    expect(mock).not.toHaveBeenCalled();
  });

  it('rejects a password that contains the user name', async () => {
    stubApi({});
    render(<AdminPage onBack={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await fill('admin', 'my-admin-password', 'my-admin-password');
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(/user name/);
  });

  it('saves the administrator and passes the user name on', async () => {
    const { sent } = stubApi({ 'PUT /appliance/api/v1/setup/admin': [204] });
    const onDone = vi.fn();
    render(<AdminPage onBack={vi.fn()} onDone={onDone} onExpired={vi.fn()} />);
    await fill('admin', 'correct horse battery', 'correct horse battery');
    expect(await vi.waitFor(() => onDone.mock.calls[0])).toEqual(['admin']);
    expect(sent['PUT /appliance/api/v1/setup/admin']).toEqual([{ user: 'admin', password: 'correct horse battery' }]);
    expect(location.href).not.toContain('correct');
  });

  it('shows a server error at its field', async () => {
    stubApi({ 'PUT /appliance/api/v1/setup/admin': [400, { code: 'invalid', field: 'password', message: 'That password is too common.' }] });
    render(<AdminPage onBack={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await fill('admin', 'correct horse battery', 'correct horse battery');
    expect(await screen.findByText('That password is too common.')).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
  });

  it('shows a generic message for a refused CSRF token and returns to the code page when the session ends', async () => {
    stubApi({ 'PUT /appliance/api/v1/setup/admin': [403, { code: 'csrf', message: 'x' }] });
    const { unmount } = render(<AdminPage onBack={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await fill('admin', 'correct horse battery', 'correct horse battery');
    expect(await screen.findByRole('alert')).toHaveTextContent('Reload the page');
    unmount();

    stubApi({ 'PUT /appliance/api/v1/setup/admin': [401, { code: 'unauthorized', message: 'x' }] });
    const onExpired = vi.fn();
    render(<AdminPage onBack={vi.fn()} onDone={vi.fn()} onExpired={onExpired} />);
    await fill('admin', 'correct horse battery', 'correct horse battery');
    await vi.waitFor(() => expect(onExpired).toHaveBeenCalled());
  });

  it('goes back to the choice', async () => {
    const onBack = vi.fn();
    render(<AdminPage onBack={onBack} onDone={vi.fn()} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(onBack).toHaveBeenCalled();
  });
});
