import type { components } from '../../api/schema';

type Bundle = components['schemas']['UpdateBundle'];
type Updates = components['schemas']['Updates'];

const blocked: Record<NonNullable<Bundle['blocked']>, string> = {
  olderThanRunning: 'This bundle is older than what runs.',
  operatorOutOfRange: "The ILM operator version that runs is outside the range this bundle works with.",
  needsAppliance: 'A newer appliance version must be installed first.',
  wrongArch: 'This bundle is for another architecture.',
  sameVersion: 'The appliance already runs this version.',
  invalidMetadata: "This bundle's metadata is invalid.",
};

/** A platform bundle's operator is checked against the range the running appliance supports. */
const platformOperatorOutOfRange = "This bundle's ILM operator version is outside the range the running appliance supports.";

/** Why a bundle of kind cannot be installed now, and which update must come first. */
export function blockedText(reason: string, needs?: string, kind?: Bundle['kind']) {
  if (reason === 'needsAppliance' && needs) return `Install appliance ${needs} first.`;
  if (reason === 'operatorOutOfRange' && kind === 'platform') {
    return needs ? `${platformOperatorOutOfRange} Install appliance ${needs} first.` : platformOperatorOutOfRange;
  }
  return (blocked as Record<string, string>)[reason] ?? 'This bundle cannot be installed now.';
}

const checkErrors: Record<NonNullable<Updates['lastCheckError']>, string> = {
  unreachable: 'The online repository cannot be reached.',
  badSignature: "A bundle's signature in the online repository did not verify.",
  timeout: 'The online repository did not answer in time.',
  failed: 'The check failed.',
};

/** Why the last check of the online repository failed. */
export function checkErrorText(reason: string) {
  return (checkErrors as Record<string, string>)[reason] ?? checkErrors.failed;
}

const loadReasons: Record<string, string> = {
  unsigned: 'The bundle is not signed.',
  badSignature: "The bundle's signature does not verify.",
  truncated: 'The bundle file is incomplete.',
  damaged: 'The bundle is damaged.',
  formatTooNew: 'The bundle needs a newer appliance to be read.',
  invalidBundle: 'The file is not a valid bundle.',
  wrongKind: 'The file is a bundle of the other kind.',
  wrongVersion: "The bundle's version is not the one asked for.",
  wrongArch: 'The bundle is for another architecture.',
  unmirroredRegistry: 'The bundle has images from a registry this appliance does not mirror.',
  noSpace: "The data volume has too little free space: it needs twice the bundle's size.",
  busy: 'Another operation is running.',
  notFound: 'The bundle was not found in the online repository.',
  unreachable: 'The online repository cannot be reached.',
  timeout: 'The online repository did not answer in time.',
  failed: 'The load failed.',
};

/** Why a load of kind was refused or failed: a reason of the load, or one of a blocked bundle. */
export function loadReasonText(reason?: string, kind?: Bundle['kind']) {
  if (!reason) return '';
  return loadReasons[reason] ?? (reason in blocked ? blockedText(reason, undefined, kind) : '');
}

const refusals: Record<string, string> = {
  no_space: "The data volume has too little free space for the bundle: it needs three times the bundle's size.",
  busy: 'Another operation is running (a backup, restore, update or bundle load). Try again once it has finished.',
  incomplete: 'The bundle did not arrive completely. Upload it again.',
  updates_unavailable: 'Update bundles are not available on this appliance.',
};

/** The text of the code an upload or fetch was refused with; an unknown code gets fallback. */
export function refusalText(code: string | undefined, fallback: string) {
  if (code === 'bundle_loading') return refusals.busy;
  return (code && refusals[code]) || fallback;
}

const updateRefusals: Record<string, string> = {
  update_running: 'Another update is running.',
  backup_running: 'A backup is running. Update once it has finished.',
  restore_running: 'A restore is running. Update once it has ended.',
  bundle_loading: refusals.busy,
  busy: refusals.busy,
  upgrade_in_progress: 'The ILM operator is upgrading the platform. Update once it has finished.',
  not_installed: 'The platform is not installed yet.',
  last_update_failed: 'The last platform update failed. Roll it back before installing another.',
  operator_older_than_running: "The bundle's ILM operator is older than the one that runs, which is never downgraded.",
  blocked: 'This bundle cannot be installed on what runs.',
  kubernetes_older_than_running: "The bundle's Kubernetes is older than the one that runs, which is never downgraded.",
  no_space: "The data volume has too little free space for the update: it needs twice the bundle's size.",
  not_found: 'The bundle is neither loaded nor found online.',
  bundle_unavailable: "The bundle's files are no longer on the appliance. Load the bundle again.",
  invalid_bundle: 'The bundle carries no installer, manager or etcd image, so it cannot be installed.',
  kubernetes_minor_skip: "The bundle's Kubernetes is more than one minor version above the one that runs. Install the system releases in between first.",
};

/** The text of the code an update of kind was refused with; an unknown code gets a general text. */
export function updateRefusalText(code?: string, kind?: Bundle['kind']) {
  // A system bundle is installed only once loaded.
  if (code === 'not_found' && kind === 'system') return 'The system bundle is not loaded.';
  return (code && updateRefusals[code]) || 'The update could not be started.';
}

const steps: Record<string, string> = {
  backup: 'taking a backup to every target',
  images: "making the bundle's images available",
  operator: "installing the bundle's ILM operator",
  version: 'switching ILM to the new version',
  wait: 'waiting for ILM to run the new version',
};

/** What an update does at step. */
export function stepText(step?: string) {
  return (step && steps[step]) || 'preparing';
}

const systemSteps: Record<string, string> = {
  backup: steps.backup,
  images: steps.images,
  manager: 'switching to the new manager',
  talos: 'upgrading Talos and rebooting the node',
  health: 'checking that the node is healthy',
  kubernetes: 'upgrading Kubernetes',
  addons: 'updating the add-ons',
  rolledBack: 'rolling Talos back',
};

/** What a system update does at step. */
export function systemStepText(step?: string) {
  return (step && systemSteps[step]) || 'preparing';
}

/** The steps of a system update in order, with what each does. */
export const systemStepOrder = ['backup', 'images', 'manager', 'talos', 'health', 'kubernetes', 'addons'] as const;

const undone = 'The update was undone before Talos changed, and the appliance runs the previous release.';

const updateFailures: Record<string, string> = {
  managerDidNotStart: `The manager of the new release did not start in time. ${undone}`,
  talosUpgradeRefused: `The node refused the Talos upgrade. ${undone}`,
  talosNotRebooted: 'The node did not boot the new Talos in time, and the update was handed back to the previous release. If Talos was installed already, the next reboot may start the new Talos.',
  talosInstalledNotRebooted: 'The new Talos is installed, but the node did not reboot into it in time. The next reboot starts it, without the health check of the update.',
  healthCheckFailed: 'The node did not come back healthy on the new Talos in time. Talos was rolled back and the appliance runs the previous release.',
  talosRollbackFailed: 'The node did not come back healthy on the new Talos, and the rollback of Talos did not take, so it runs the new Talos.',
  backupFailed: 'The backup to every target did not complete.',
  backupIncomplete: 'The backup does not include the database, which could not be reached. Update once it answers again.',
  bundleUnavailable: 'The bundle is no longer loaded and could not be fetched.',
  imagesMissing: "Some of the bundle's images are missing from the appliance's registry.",
  versionRejected: 'The ILM operator did not accept the new version.',
  noSpace: 'The data volume has too little free space.',
  timeout: 'The step did not finish in time.',
  platformUpgrading: 'The ILM operator was upgrading the platform, so the new ILM version was not asked for.',
  notInstalled: 'The platform is not installed.',
  failed: 'A step failed. The manager log has the details.',
};

/** Why an update failed, from its fixed reason. */
export function updateFailureText(reason: string) {
  return updateFailures[reason] ?? 'The manager log has the details.';
}

const rollbackRefusals: Record<string, string> = {
  no_pre_update_backup: 'There is no platform update to roll back, or the backup taken before it is gone.',
  no_previous_bundle: 'The platform bundle that ran before the update is no longer on the appliance, and an older bundle cannot be loaded. Restore the backup taken before the update onto a fresh appliance instead.',
  operator_changed: 'The update installed a newer ILM operator, which is never downgraded. Restore the backup onto a fresh appliance instead.',
  not_needed: 'The update failed before it asked for the new ILM version, so ILM still runs the previous version. A rollback would only lose the data changed since the backup.',
  external_database: 'The platform uses an external database, which a rollback cannot restore. Restore the backup onto a fresh appliance instead.',
  foreign_kit: 'The kit does not open the backup taken before the update: the backup was encrypted to another backup key.',
  backup_broken: 'The backup taken before the update cannot be restored: it is damaged, too new, or holds another ILM version.',
  update_running: 'Another update or rollback is running.',
  backup_running: 'A backup is running. Roll back once it has finished.',
  restore_running: 'A restore is running. Roll back once it has ended.',
  bundle_loading: refusals.busy,
  busy: refusals.busy,
  upgrade_in_progress: 'The ILM operator is upgrading the platform. Roll back once it has finished.',
  not_installed: updateRefusals.not_installed,
};

/** The text of the code a rollback was refused with; undefined for a code the console does not know. */
export function rollbackRefusalText(code?: string) {
  return code ? rollbackRefusals[code] : undefined;
}

const rollbackUnavailable: Record<string, string> = {
  notNeeded: "The last update failed before it asked for the new ILM version, so there is nothing to roll back: ILM still runs the previous version. The bundle's newer ILM operator may already run, which is never downgraded.",
  externalDatabase: 'The last update cannot be rolled back: the platform uses an external database, which a rollback cannot restore. Restore the backup taken before it onto a fresh appliance instead.',
  operatorChanged: 'The last update cannot be rolled back: it installed a newer ILM operator, which is never downgraded. The way back is to restore the backup taken before it onto a fresh appliance.',
  noPreviousBundle: 'The last update cannot be rolled back: the platform bundle that ran before it is no longer on the appliance, and an older bundle cannot be loaded. Restore the backup taken before it onto a fresh appliance instead.',
  upgradeInProgress: 'The ILM operator is upgrading the platform. The last update can be rolled back once it has finished.',
};

/** Why the last platform update cannot be rolled back, for the reasons worth telling; undefined for the others. */
export function rollbackUnavailableText(reason?: string) {
  return reason ? rollbackUnavailable[reason] : undefined;
}

const rollbackSteps: Record<string, string> = {
  stop: 'stopping ILM',
  uninstall: 'uninstalling the platform',
  reinstall: 'installing ILM again at the previous version',
  ilmSecrets: "writing ILM's secrets from the backup",
  database: "loading the backup's database",
  cbom: 'writing the stored CBOMs',
  start: 'starting ILM',
};

/** What a rollback does at step, a step of the restore that does it. */
export function rollbackStepText(step?: string) {
  return (step && rollbackSteps[step]) || 'preparing';
}

const rollbackFailures: Record<string, string> = {
  failed: 'a step failed; the manager log has the details',
  timeout: 'a step did not finish in time',
};

/** Why a rollback stopped, from the restore's fixed reason. */
export function rollbackFailureText(reason: string) {
  return rollbackFailures[reason] ?? rollbackFailures.failed;
}
