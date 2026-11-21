import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { AccessPage } from './AccessPage';

const list = (items: object[]) => [200, { items }] as const;
const viewer = {
  kind: 'kubeconfig', name: 'kc-abc12', role: 'view', issuedBy: 'admin',
  issuedAt: '2026-11-21T10:00:00Z', expires: '2026-11-21T18:00:00Z',
};
const pipeline = { kind: 'kubeconfig', name: 'named-ci', role: 'cluster-admin', issuedBy: 'admin', issuedAt: '2026-11-20T10:00:00Z' };
const reader = { kind: 'talosconfig', role: 'os:reader', issuedBy: 'ops', issuedAt: '2026-11-21T09:00:00Z', expires: '2026-11-21T11:00:00Z' };
const click = vi.fn();

beforeEach(() => {
  URL.createObjectURL = vi.fn(() => 'blob:file');
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

describe('AccessPage', () => {
  it('lists kubeconfigs with their expiry and talosconfigs without a revoke action', async () => {
    stubApi({ 'GET /appliance/api/v1/access': list([viewer, pipeline, reader]) });
    render(<AccessPage />);
    expect(await screen.findByText('kc-abc12')).toBeInTheDocument();
    expect(screen.getByText('named-ci')).toBeInTheDocument();
    expect(screen.getByText('Valid until revoked')).toBeInTheDocument();
    expect(screen.getByText('os:reader')).toBeInTheDocument();
    expect(screen.getByText(/issued by ops/i)).toBeInTheDocument();
    expect(screen.getByText(/talosconfigs cannot be revoked/i)).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^Revoke/ })).toHaveLength(2);
  });

  it('marks the break-glass kubeconfig of the recovery kit and offers no revoke action for it', async () => {
    const breakGlass = { kind: 'kubeconfig', name: 'break-glass', role: 'cluster-admin', issuedBy: 'admin', issuedAt: '2026-11-21T10:00:00Z', breakGlass: true };
    stubApi({ 'GET /appliance/api/v1/access': list([breakGlass, pipeline]) });
    render(<AccessPage />);
    expect(await screen.findByText('break-glass')).toBeInTheDocument();
    expect(screen.getByText(/issuing the recovery kit again revokes it/i)).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^Revoke/ })).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Revoke break-glass' })).not.toBeInTheDocument();
  });

  it('says when nothing is issued and reports a list that cannot be read', async () => {
    stubApi({ 'GET /appliance/api/v1/access': list([]) });
    const first = render(<AccessPage />);
    expect(await screen.findByText(/no kubeconfigs are issued/i)).toBeInTheDocument();
    expect(screen.getByText(/no talosconfigs are valid/i)).toBeInTheDocument();
    first.unmount();
    stubApi({ 'GET /appliance/api/v1/access': [500, { code: 'internal', message: 'The request failed.' }] });
    render(<AccessPage />);
    expect(await screen.findByRole('alert')).toHaveTextContent('The request failed.');
  });

  it('issues a time-limited kubeconfig, downloads it and lists it', async () => {
    let items: object[] = [];
    const { mock, sent } = stubApi({
      'GET /appliance/api/v1/access': () => list(items),
      'POST /appliance/api/v1/access/kubeconfig': () => {
        items = [viewer];
        return [200, 'apiVersion: v1\nkind: Config\n'];
      },
    });
    render(<AccessPage />);
    await screen.findByText(/no kubeconfigs/i);
    const hours = screen.getByLabelText('Hours', { selector: '[name="hours"]' });
    await userEvent.clear(hours);
    await userEvent.type(hours, '2');
    await userEvent.click(screen.getByRole('button', { name: 'Issue the kubeconfig' }));
    expect(await screen.findByText(/kubeconfig.yaml was downloaded/)).toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/access/kubeconfig']).toEqual([{ role: 'view', hours: 2 }]);
    expect(click).toHaveBeenCalledWith('kubeconfig.yaml', 'blob:file');
    expect(await screen.findByText('kc-abc12')).toBeInTheDocument();
    expect(mock).toHaveBeenCalledTimes(3);
  });

  it('issues a long-lived kubeconfig with a name and the file name the appliance gives', async () => {
    const { mock, sent } = stubApi({ 'GET /appliance/api/v1/access': list([]) });
    render(<AccessPage />);
    await screen.findByText(/no kubeconfigs/i);
    mock.mockImplementationOnce(async () => new Response('kind: Config\n', {
      status: 200, headers: { 'Content-Type': 'application/yaml', 'Content-Disposition': 'attachment; filename="kubeconfig-named-ci.yaml"' },
    }));
    await userEvent.click(screen.getByLabelText(/cluster admin/i));
    await userEvent.click(screen.getByLabelText(/long-lived/i));
    const submit = screen.getByRole('button', { name: 'Issue the kubeconfig' });
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByLabelText('Name'), 'ci');
    await userEvent.click(submit);
    expect(await screen.findByText(/kubeconfig-named-ci.yaml was downloaded/)).toBeInTheDocument();
    expect(click).toHaveBeenCalledWith('kubeconfig-named-ci.yaml', 'blob:file');
    expect(sent['POST /appliance/api/v1/access/kubeconfig']).toBeUndefined();
  });

  it('shows a refused name at the field and other refusals above the form', async () => {
    stubApi({
      'GET /appliance/api/v1/access': list([]),
      'POST /appliance/api/v1/access/kubeconfig': [409, { code: 'exists', message: 'A kubeconfig with this name exists.' }],
    });
    render(<AccessPage />);
    await screen.findByText(/no kubeconfigs/i);
    await userEvent.click(screen.getByLabelText(/long-lived/i));
    await userEvent.type(screen.getByLabelText('Name'), 'ci');
    await userEvent.click(screen.getByRole('button', { name: 'Issue the kubeconfig' }));
    expect(await screen.findByText('A kubeconfig with this name exists.')).toBeInTheDocument();
    expect(click).not.toHaveBeenCalled();
  });

  it('issues a talosconfig and shows a refused number of hours at its field', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/access': list([]),
      'POST /appliance/api/v1/access/talosconfig': (body) => (body as { hours: number }).hours > 8
        ? [400, { code: 'invalid', message: 'Choose 1 to 8 hours.', field: 'hours' }]
        : [200, 'context: appliance\n'],
    });
    render(<AccessPage />);
    await screen.findByText(/no talosconfigs/i);
    const hours = screen.getByLabelText('Hours', { selector: '[name="talos-hours"]' });
    await userEvent.clear(hours);
    await userEvent.type(hours, '9');
    await userEvent.click(screen.getByLabelText(/^Admin/));
    await userEvent.click(screen.getByRole('button', { name: 'Issue the talosconfig' }));
    expect(await screen.findByText('Choose 1 to 8 hours.')).toBeInTheDocument();
    await userEvent.clear(hours);
    await userEvent.type(hours, '4');
    await userEvent.click(screen.getByRole('button', { name: 'Issue the talosconfig' }));
    expect(await screen.findByText(/talosconfig.yaml was downloaded/)).toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/access/talosconfig']).toEqual([{ role: 'os:admin', hours: 9 }, { role: 'os:admin', hours: 4 }]);
  });

  it('reports an appliance that cannot be reached', async () => {
    const { mock } = stubApi({ 'GET /appliance/api/v1/access': list([]) });
    render(<AccessPage />);
    await screen.findByText(/no talosconfigs/i);
    mock.mockRejectedValueOnce(new TypeError('offline'));
    await userEvent.click(screen.getByRole('button', { name: 'Issue the talosconfig' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });

  it('revokes a kubeconfig after confirmation', async () => {
    let items: object[] = [viewer];
    const { sent } = stubApi({
      'GET /appliance/api/v1/access': () => list(items),
      'DELETE /appliance/api/v1/access/kubeconfig/kc-abc12': () => {
        items = [];
        return [204];
      },
    });
    render(<AccessPage />);
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke kc-abc12' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(/stops working at once/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    expect(await screen.findByText(/no kubeconfigs are issued/i)).toBeInTheDocument();
    expect(sent['DELETE /appliance/api/v1/access/kubeconfig/kc-abc12']).toHaveLength(1);
  });
});
