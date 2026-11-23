import type { components } from '../../api/schema';

export type Reason = components['schemas']['BackupTargetReason'];

const reasons: Record<Reason, string> = {
  unreachable: 'The target cannot be reached from the appliance or through its proxy.',
  authFailed: 'The target refused the credentials.',
  hostKeyMismatch: "The server's host key does not match the accepted one.",
  notFound: 'The bucket, directory or export does not exist.',
  noSpace: 'The target has no space left.',
  timeout: 'The target did not answer in time.',
  failed: 'The copy failed.',
};

/** The text of a fixed reason a target failed with. */
export function reasonText(reason: Reason) {
  return reasons[reason];
}
