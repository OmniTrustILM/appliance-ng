import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubApi } from '../../setup/test-utils';
import { TrustPage } from './TrustPage';

const sha = (digit: string) => digit.repeat(64);
const ca = (name: string, digit: string, over: object = {}) => ({
  subject: `CN=${name}`,
  issuer: 'CN=Corp Root',
  notAfter: '2035-01-02T00:00:00Z',
  sha256: sha(digit),
  ...over,
});
const pem = '-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----';

function renderPage() {
  render(<MemoryRouter><TrustPage /></MemoryRouter>);
}

afterEach(() => vi.unstubAllGlobals());

describe('TrustPage', () => {
  it('lists the trusted CAs with a shortened fingerprint that stays readable in full', async () => {
    stubApi({ 'GET /appliance/api/v1/trust': [200, { items: [ca('Corp Root', 'a')] }] });
    renderPage();
    expect(await screen.findByText('CN=Corp Root')).toBeInTheDocument();
    expect(screen.getByText('Issuer: CN=Corp Root')).toBeInTheDocument();
    expect(screen.getByText(/Expires:/)).toBeInTheDocument();
    expect(screen.getByText(`${'a'.repeat(16)}…`)).toHaveAttribute('title', sha('a'));
    expect(screen.getByText(sha('a'))).toHaveClass('sr-only');
  });

  it('says when no CA is trusted and reports a list that cannot be read', async () => {
    stubApi({ 'GET /appliance/api/v1/trust': [200, { items: [] }] });
    const first = render(<MemoryRouter><TrustPage /></MemoryRouter>);
    expect(await screen.findByText(/no additional ca certificates/i)).toBeInTheDocument();
    first.unmount();
    stubApi({ 'GET /appliance/api/v1/trust': [409, { code: 'conflict', message: 'Setup is not done.' }] });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup is not done.');
  });

  it('offers adding only after a preview, shows what is new and already trusted, and sends the bundle', async () => {
    let items = [ca('Corp Root', 'a')];
    const { sent } = stubApi({
      'GET /appliance/api/v1/trust': () => [200, { items }],
      'POST /appliance/api/v1/trust/preview': [200, { items: [ca('Corp Root', 'a', { new: false }), ca('Team CA', 'b', { new: true })] }],
      'POST /appliance/api/v1/trust': () => {
        items = [...items, ca('Team CA', 'b')];
        return [202];
      },
    });
    renderPage();
    await screen.findByText('CN=Corp Root');
    expect(screen.getByRole('button', { name: 'Preview' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: /add the new/i })).not.toBeInTheDocument();

    await userEvent.click(screen.getByLabelText('Or paste the certificates'));
    await userEvent.paste(pem);
    expect(screen.queryByRole('button', { name: /add the new/i })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));

    expect(await screen.findByRole('heading', { name: 'New certificates (1)' })).toBeInTheDocument();
    expect(screen.getByText('1 already trusted and not added again.')).toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/trust']).toBeUndefined();
    expect(sent['POST /appliance/api/v1/trust/preview']).toEqual([{ pem }]);

    await userEvent.click(screen.getByRole('button', { name: 'Add the new certificates' }));
    expect(await screen.findByText(/needs a restart/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'restart the node' })).toHaveAttribute('href', '/system');
    expect(sent['POST /appliance/api/v1/trust']).toEqual([{ pem }]);
    expect(await screen.findAllByText('CN=Team CA')).not.toHaveLength(0);
    expect(screen.getByLabelText('Or paste the certificates')).toHaveValue('');
  });

  it('withdraws the preview when the bundle changes', async () => {
    stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [] }],
      'POST /appliance/api/v1/trust/preview': [200, { items: [ca('Team CA', 'b', { new: true })] }],
    });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    expect(await screen.findByRole('button', { name: 'Add the new certificates' })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'y');
    expect(screen.queryByRole('button', { name: 'Add the new certificates' })).not.toBeInTheDocument();
  });

  it('ignores a preview that arrives after the bundle was edited', async () => {
    let answer: (reply: Response) => void = () => undefined;
    const { mock } = stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [] }],
      'POST /appliance/api/v1/trust': [202],
    });
    const fallback = mock.getMockImplementation();
    mock.mockImplementation(async (request: Request) => {
      if (new URL(request.url).pathname !== '/appliance/api/v1/trust/preview') return fallback?.(request) ?? new Response(null, { status: 404 });
      return new Promise<Response>((resolve) => { answer = resolve; });
    });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'y');
    answer(new Response(JSON.stringify({ items: [ca('Team CA', 'b', { new: true })] }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Preview' })).toBeEnabled());
    expect(screen.queryByText('CN=Team CA')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add the new certificates' })).not.toBeInTheDocument();
  });

  it('adds the previewed bundle', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [] }],
      'POST /appliance/api/v1/trust/preview': [200, { items: [ca('Team CA', 'b', { new: true })] }],
      'POST /appliance/api/v1/trust': [202],
    });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Add the new certificates' }));
    expect(sent['POST /appliance/api/v1/trust']).toEqual([{ pem: 'x' }]);
  });

  it('shows the server reason for a rejected upload and adds nothing', async () => {
    const { sent } = stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [] }],
      'POST /appliance/api/v1/trust/preview': [400, { code: 'invalid', message: 'CN=leaf is not a CA certificate.' }],
    });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('CN=leaf is not a CA certificate. Nothing was added.');
    expect(screen.queryByRole('button', { name: /add the new/i })).not.toBeInTheDocument();
    expect(sent['POST /appliance/api/v1/trust']).toBeUndefined();
  });

  it('says nothing is new when every certificate is already trusted', async () => {
    stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [ca('Corp Root', 'a')] }],
      'POST /appliance/api/v1/trust/preview': [200, { items: [ca('Corp Root', 'a')] }],
    });
    renderPage();
    await screen.findByText('CN=Corp Root');
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    expect(await screen.findByText(/already trusted\./i, { selector: 'p' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /add the new/i })).not.toBeInTheDocument();
  });

  it('reports an unreachable appliance and a refused add', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [] }],
      'POST /appliance/api/v1/trust/preview': [200, { items: [ca('Team CA', 'b', { new: true })] }],
      'POST /appliance/api/v1/trust': [409, { code: 'conflict', message: 'Setup is not done.' }],
    });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Add the new certificates' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Setup is not done. Nothing was added.');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    expect(await screen.findByText(/cannot be reached.*Nothing was added/i)).toBeInTheDocument();
  });

  it('does not claim an add that was not confirmed failed', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [] }],
      'POST /appliance/api/v1/trust/preview': [200, { items: [ca('Team CA', 'b', { new: true })] }],
      'POST /appliance/api/v1/trust': [500, { code: 'internal', message: 'Internal error.' }],
    });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Add the new certificates' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/not known whether the certificates were added/);
    expect(alert).not.toHaveTextContent('Nothing was added');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: 'Add the new certificates' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/cannot be reached.*not known whether the certificates were added/));
  });

  it('keeps the latest chosen file when an earlier one is read later', async () => {
    stubApi({ 'GET /appliance/api/v1/trust': [200, { items: [] }] });
    renderPage();
    await screen.findByText(/no additional/i);
    let finish: (text: string) => void = () => undefined;
    const slow = new File(['slow'], 'slow.pem');
    Object.defineProperty(slow, 'text', { value: () => new Promise<string>((resolve) => { finish = resolve; }) });
    await userEvent.upload(screen.getByLabelText(/^Certificate file/), slow);
    await userEvent.upload(screen.getByLabelText(/^Certificate file/), new File(['fast'], 'fast.pem'));
    await screen.findByDisplayValue('fast');
    finish('slow');
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.getByLabelText('Or paste the certificates')).toHaveValue('fast');
    expect(screen.getByText('fast.pem')).toBeInTheDocument();
  });

  it('keeps text typed while a chosen file is read', async () => {
    stubApi({ 'GET /appliance/api/v1/trust': [200, { items: [] }] });
    renderPage();
    await screen.findByText(/no additional/i);
    let finish: (text: string) => void = () => undefined;
    const slow = new File(['slow'], 'slow.pem');
    Object.defineProperty(slow, 'text', { value: () => new Promise<string>((resolve) => { finish = resolve; }) });
    await userEvent.upload(screen.getByLabelText(/^Certificate file/), slow);
    await userEvent.type(screen.getByLabelText('Or paste the certificates'), 'typed');
    finish('slow');
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.getByLabelText('Or paste the certificates')).toHaveValue('typed');
  });

  it('fills the bundle from a chosen or dropped file', async () => {
    stubApi({ 'GET /appliance/api/v1/trust': [200, { items: [] }] });
    renderPage();
    await screen.findByText(/no additional/i);
    await userEvent.upload(screen.getByLabelText(/^Certificate file/), new File([pem], 'ca.pem'));
    await waitFor(() => expect(screen.getByLabelText('Or paste the certificates')).toHaveValue(pem));
    fireEvent.drop(screen.getByTestId('drop-zone'), { dataTransfer: { files: [new File(['dropped'], 'd.pem')] } });
    expect(await screen.findByDisplayValue('dropped')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Preview' })).toBeEnabled();
  });

  it('removes a CA after the confirmation and points to the restart', async () => {
    let items = [ca('Corp Root', 'a'), ca('Team CA', 'b')];
    const { sent } = stubApi({
      'GET /appliance/api/v1/trust': () => [200, { items }],
      [`DELETE /appliance/api/v1/trust/${sha('a')}`]: () => {
        items = items.slice(1);
        return [202];
      },
    });
    renderPage();
    await screen.findByText('CN=Corp Root');
    await userEvent.click(screen.getByRole('button', { name: /Remove CN=Corp Root/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(sent[`DELETE /appliance/api/v1/trust/${sha('a')}`]).toBeUndefined();
    await userEvent.click(screen.getByRole('button', { name: /Remove CN=Corp Root/ }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText(/needs a restart/i)).toBeInTheDocument();
    expect(sent[`DELETE /appliance/api/v1/trust/${sha('a')}`]).toHaveLength(1);
    expect(screen.queryByText('CN=Corp Root')).not.toBeInTheDocument();
  });

  it('reports a CA that cannot be removed', async () => {
    const { mock } = stubApi({
      'GET /appliance/api/v1/trust': [200, { items: [ca('Corp Root', 'a')] }],
      [`DELETE /appliance/api/v1/trust/${sha('a')}`]: [404, { code: 'not_found', message: 'That certificate is not trusted.' }],
    });
    renderPage();
    await screen.findByText('CN=Corp Root');
    await userEvent.click(screen.getByRole('button', { name: /Remove/ }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('That certificate is not trusted.');
    mock.mockRejectedValue(new TypeError('down'));
    await userEvent.click(screen.getByRole('button', { name: /Remove/ }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText(/cannot be reached/i)).toBeInTheDocument();
  });
});
