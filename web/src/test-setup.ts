import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

afterEach(cleanup);

// jsdom has no matchMedia; the theme code reads it. Report "no match" unless a test stubs it.
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  configurable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }),
});

// jsdom's FormData and File are not the ones fetch's Request understands, so a multipart body would not
// survive. The tests use Node's: its FormData class is the one an empty form read from a response has.
// The module name is a variable because the project has no Node type declarations.
const nodeBuffer = 'node:buffer';
const { File: NodeFile } = (await import(/* @vite-ignore */ nodeBuffer)) as { File: typeof File };
globalThis.FormData = (await new Response(new URLSearchParams()).formData()).constructor as typeof FormData;
globalThis.File = NodeFile;

// jsdom has no <dialog> methods. Mirror the browser's: showModal opens the dialog and lets Escape fire a
// cancelable cancel event, close closes it and fires close.
const modals = new Set<HTMLDialogElement>();
document.addEventListener('keydown', (event) => {
  if (event.key !== 'Escape') return;
  const dialog = [...modals].pop();
  if (dialog?.dispatchEvent(new Event('cancel', { cancelable: true }))) dialog.close();
});
HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
  this.setAttribute('open', '');
  modals.add(this);
};
HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
  modals.delete(this);
  if (!this.hasAttribute('open')) return;
  this.removeAttribute('open');
  this.dispatchEvent(new Event('close'));
};
