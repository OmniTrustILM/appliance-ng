import { api, timeoutSignal } from '../api/client';
import { failureMessage, unreachable } from '../setup/failure';

/** How long a request that reads a whole backup may take: a fetch, a check with the kit or a resume. */
export const restoreTimeoutMs = 60 * 60_000;

/** Why a restore request was refused, with the field it concerns. */
export type Refusal = { message: string; field?: string };

type Result = { response: Response; error?: { message: string; field?: string } } | undefined;

/** The refusal of a request's result; undefined when it succeeded. An unreachable appliance is a refusal too. */
export function refusalOf(result: Result): Refusal | undefined {
  if (!result) return { message: unreachable };
  if (result.response.ok) return undefined;
  return { message: result.error?.message ?? failureMessage(result.response.status), field: result.error?.field };
}

function kitForm(kit: File, passphrase: string, fields: Record<string, string> = {}) {
  const form = new FormData();
  for (const [key, value] of Object.entries(fields)) form.append(key, value);
  form.append('kit', kit);
  form.append('passphrase', passphrase);
  return form;
}

/** Checks a backup, uploaded or local, with the recovery kit; the appliance keeps its key for this session. */
export function inspectBackup(source: 'upload' | 'local', name: string, kit: File, passphrase: string) {
  return api.POST('/restore/inspect', { body: kitForm(kit, passphrase, { source, name }) as never, signal: timeoutSignal(restoreTimeoutMs) });
}

/** Resumes the restore that waits for its kit, or one that failed. */
export function resumeRestore(kit: File, passphrase: string) {
  return api.POST('/restore/resume', { body: kitForm(kit, passphrase) as never, signal: timeoutSignal(restoreTimeoutMs) });
}

/** Uploads file as the backup to restore; the upload runs as long as it keeps sending. */
export function uploadBackup(file: File) {
  return api.PUT('/restore/upload', {
    params: { query: { name: file.name } },
    body: file as never,
    bodySerializer: (body: unknown) => body as BodyInit,
    headers: { 'Content-Type': 'application/octet-stream' },
  });
}
