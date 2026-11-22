import { render, screen } from '@testing-library/react';
import { BrowserRouter, Link } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import App from './App';
import { apiBase, basePath, routerBasename } from './base';

describe('console base', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.history.replaceState(null, '', '/');
  });

  it('serves the console and its API under /appliance/', () => {
    expect(basePath).toBe('/appliance');
    expect(apiBase).toBe('/appliance/api/v1');
  });

  it('routes the console under the base path', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ code: 'unauthorized', message: 'No session.' }), {
      status: 401,
      headers: { 'Content-Type': 'application/json' },
    })));
    window.history.replaceState(null, '', '/appliance/signin');
    render(<BrowserRouter basename={routerBasename}><App /></BrowserRouter>);
    expect(await screen.findByRole('heading', { name: 'Sign in to the appliance' })).toBeInTheDocument();
  });

  it('keeps the trailing slash on the console root', async () => {
    window.history.replaceState(null, '', '/appliance/signin');
    render(<BrowserRouter basename={routerBasename}><Link to="/">Dashboard</Link></BrowserRouter>);
    expect(screen.getByRole('link', { name: 'Dashboard' })).toHaveAttribute('href', '/appliance/');
  });
});
