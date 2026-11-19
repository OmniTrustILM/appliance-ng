import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { SystemPage } from './SystemPage';

afterEach(() => vi.unstubAllGlobals());

const status = (rebootRequired: boolean) => [200, { setup: 'complete', conditions: [], versions: {}, rebootRequired }] as const;

describe('SystemPage', () => {
  it('says whether a restart is needed', async () => {
    stubApi({ 'GET /api/v1/status': status(true) });
    render(<SystemPage />);
    expect(await screen.findByText(/needs to restart/i)).toBeInTheDocument();
  });

  it('says that no restart is needed', async () => {
    stubApi({ 'GET /api/v1/status': status(false) });
    render(<SystemPage />);
    expect(await screen.findByText('No restart is needed.')).toBeInTheDocument();
  });

  it('says nothing about a restart until the status is read, and reports a status that cannot be read', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('down')));
    render(<SystemPage />);
    expect(screen.queryByText('No restart is needed.')).not.toBeInTheDocument();
    expect(await screen.findByRole('alert')).toHaveTextContent(/cannot be reached/i);
    expect(screen.queryByText('No restart is needed.')).not.toBeInTheDocument();
  });

  it('cancels with Escape after a click on the dialog text', async () => {
    stubApi({ 'GET /api/v1/status': status(false) });
    render(<SystemPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await userEvent.click(screen.getByText(/unavailable until the node is back/));
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('restarts after the confirmation', async () => {
    const { sent } = stubApi({ 'GET /api/v1/status': status(false), 'POST /api/v1/system/reboot': [202] });
    render(<SystemPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    const dialog = screen.getByRole('dialog', { name: 'Restart the node?' });
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Restart' }));
    expect(await screen.findByText(/is restarting/i)).toBeInTheDocument();
    expect(sent['POST /api/v1/system/reboot']).toEqual([{ confirm: true }]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByText(/is restarting/i).closest('[tabindex="-1"]')).toHaveFocus();
  });

  it('sends nothing when the dialog is cancelled, by button or Escape', async () => {
    const { sent } = stubApi({ 'GET /api/v1/status': status(false) });
    render(<SystemPage />);
    const open = screen.getByRole('button', { name: 'Restart' });
    await userEvent.click(open);
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(open).toHaveFocus();
    await userEvent.click(open);
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(sent['POST /api/v1/system/reboot']).toBeUndefined();
  });

  it('keeps the focus inside the dialog', async () => {
    stubApi({ 'GET /api/v1/status': status(false) });
    render(<SystemPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await userEvent.tab();
    expect(within(screen.getByRole('dialog')).getAllByRole('button')[1]).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await userEvent.tab({ shift: true });
    expect(within(screen.getByRole('dialog')).getAllByRole('button')[1]).toHaveFocus();
  });

  it('shuts down after the confirmation and says the console goes away', async () => {
    const { sent } = stubApi({ 'GET /api/v1/status': status(false), 'POST /api/v1/system/shutdown': [202] });
    render(<SystemPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Shut down' }));
    await userEvent.click(within(screen.getByRole('dialog', { name: 'Shut the node down?' })).getByRole('button', { name: 'Shut down' }));
    expect(await screen.findByText(/console goes away/i)).toBeInTheDocument();
    expect(sent['POST /api/v1/system/shutdown']).toEqual([{ confirm: true }]);
  });

  it('reports an unreachable node', async () => {
    stubApi({ 'GET /api/v1/status': status(false), 'POST /api/v1/system/reboot': [503, { code: 'unavailable', message: 'x' }] });
    render(<SystemPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restart' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/node cannot be reached/i);
  });

  it('reports the message of another failure and a lost connection', async () => {
    const { mock } = stubApi({ 'GET /api/v1/status': status(false), 'POST /api/v1/system/reboot': [403, { code: 'forbidden', message: 'Not allowed.' }] });
    render(<SystemPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restart' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Not allowed.');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restart' }));
    expect(await screen.findByText(/appliance cannot be reached/i)).toBeInTheDocument();
  });
});
