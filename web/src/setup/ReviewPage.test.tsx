import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ReviewPage } from './ReviewPage';
import { hang, stubApi } from './test-utils';

const answers = {
  user: 'admin',
  node: {
    hostName: 'node1.example.com',
    address: '10.0.0.5/24',
    gateway: '10.0.0.1',
    dns: ['9.9.9.9'],
    ntp: ['time.example.com'],
    proxy: { http: 'http://proxy.example.com:3128' },
  },
};

afterEach(() => vi.unstubAllGlobals());

describe('ReviewPage', () => {
  it('summarises the answers without the password or the seed', () => {
    render(<ReviewPage answers={answers} onBack={vi.fn()} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(screen.getByText('admin')).toBeInTheDocument();
    expect(screen.getByText('node1.example.com')).toBeInTheDocument();
    expect(screen.getByText('10.0.0.5/24')).toBeInTheDocument();
    expect(screen.getByText('10.0.0.1')).toBeInTheDocument();
    expect(screen.getByText('9.9.9.9')).toBeInTheDocument();
    expect(screen.getByText('http://proxy.example.com:3128')).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/password|seed/i);
  });

  it('says DHCP is kept when no address is set', () => {
    render(<ReviewPage answers={{ ...answers, node: { ...answers.node, address: '', gateway: '' } }} onBack={vi.fn()} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(screen.getByText(/Address from DHCP/)).toBeInTheDocument();
  });

  it('applies and moves on', async () => {
    stubApi({ 'POST /appliance/api/v1/setup/apply': [202] });
    const onDone = vi.fn();
    render(<ReviewPage answers={answers} onBack={vi.fn()} onChangeMode={vi.fn()} onDone={onDone} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalled());
  });

  it('goes back to the node settings', async () => {
    const onBack = vi.fn();
    render(<ReviewPage answers={answers} onBack={onBack} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Change the settings' }));
    expect(onBack).toHaveBeenCalled();
  });

  it('can still apply after a reload, when the answers are not known to the page', async () => {
    stubApi({ 'POST /appliance/api/v1/setup/apply': [202] });
    const onDone = vi.fn();
    render(<ReviewPage onBack={vi.fn()} onChangeMode={vi.fn()} onDone={onDone} onExpired={vi.fn()} />);
    expect(screen.getByText(/saved before the page was reloaded/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalled());
  });

  it('shows a generic message when the apply fails', async () => {
    stubApi({ 'POST /appliance/api/v1/setup/apply': [409, { code: 'conflict', message: 'x' }] });
    render(<ReviewPage answers={answers} onBack={vi.fn()} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Reload the page');
  });

  it('shows the chosen mode and lets it be changed', async () => {
    const onChangeMode = vi.fn();
    render(<ReviewPage answers={answers} mode="restore" onBack={vi.fn()} onChangeMode={onChangeMode} onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(screen.getByText('Set up')).toBeInTheDocument();
    expect(screen.getByText('Restore from a backup')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Change' }));
    expect(onChangeMode).toHaveBeenCalled();
  });

  it('keeps the mode until the apply has settled', async () => {
    stubApi({ 'POST /appliance/api/v1/setup/apply': hang });
    render(<ReviewPage answers={answers} mode="new" onBack={vi.fn()} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.getByRole('button', { name: 'Change' })).toBeDisabled();
  });

  it('names a new appliance', () => {
    render(<ReviewPage answers={answers} mode="new" onBack={vi.fn()} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(screen.getByText('New appliance')).toBeInTheDocument();
  });

  it('offers the change without a value when the mode is unknown after a reload', () => {
    render(<ReviewPage onBack={vi.fn()} onChangeMode={vi.fn()} onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(screen.getByRole('button', { name: 'Change' })).toBeInTheDocument();
    expect(screen.queryByText('New appliance')).not.toBeInTheDocument();
  });
});
