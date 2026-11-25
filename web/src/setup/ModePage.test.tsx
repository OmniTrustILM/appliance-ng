import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ModePage } from './ModePage';
import { stubApi } from './test-utils';

afterEach(() => vi.unstubAllGlobals());

const path = 'PUT /appliance/api/v1/setup/mode';

describe('ModePage', () => {
  it('offers a new appliance first and sends that choice', async () => {
    const { sent } = stubApi({ [path]: [204] });
    const onDone = vi.fn();
    render(<ModePage onDone={onDone} onExpired={vi.fn()} />);
    expect(screen.getByRole('radio', { name: /New appliance/ })).toBeChecked();
    expect(screen.getByRole('radio', { name: /Restore from a backup/ })).not.toBeChecked();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith('new'));
    expect(sent[path]).toEqual([{ mode: 'new' }]);
  });

  it('sends the restore choice', async () => {
    const { sent } = stubApi({ [path]: [204] });
    const onDone = vi.fn();
    render(<ModePage onDone={onDone} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('radio', { name: /Restore from a backup/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith('restore'));
    expect(sent[path]).toEqual([{ mode: 'restore' }]);
  });

  it('returns to the code page when the session ends', async () => {
    stubApi({ [path]: [401, { code: 'unauthorized', message: 'x' }] });
    const onExpired = vi.fn();
    render(<ModePage onDone={vi.fn()} onExpired={onExpired} />);
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await vi.waitFor(() => expect(onExpired).toHaveBeenCalled());
  });

  it('shows a message when the choice is not saved', async () => {
    stubApi({ [path]: [409, { code: 'conflict', message: 'x' }] });
    const onDone = vi.fn();
    render(<ModePage onDone={onDone} onExpired={vi.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(onDone).not.toHaveBeenCalled();
  });

  it('preselects the earlier choice', () => {
    render(<ModePage initial="restore" onDone={vi.fn()} onExpired={vi.fn()} />);
    expect(screen.getByRole('radio', { name: /Restore from a backup/ })).toBeChecked();
  });
});
