import { useState } from 'react';
import { Button } from './Button';

/** The ILM administrator's user name and password, shown once, with a button that copies the password. */
export function AdminPassword({ user, password }: Readonly<{ user: string; password: string }>) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(password);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-divider bg-surface-sunken px-4 py-3.5">
        <span className="break-all">ILM administrator: <code>{user}</code> / <code>{password}</code></span>
        <Button variant="secondary" onClick={() => void copy()}>{copied ? 'Copied' : 'Copy'}</Button>
      </div>
      <p className="mt-2 text-content-muted">It is not shown again.</p>
    </>
  );
}
