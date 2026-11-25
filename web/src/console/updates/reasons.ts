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

/** Why a bundle cannot be installed now, and which update must come first. */
export function blockedText(reason: string, needs?: string) {
  if (reason === 'needsAppliance' && needs) return `Install appliance ${needs} first.`;
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
  noSpace: "The data volume has too little free space: it needs twice the bundle's size.",
  busy: 'Another operation is running.',
  notFound: 'The bundle was not found in the online repository.',
  unreachable: 'The online repository cannot be reached.',
  timeout: 'The online repository did not answer in time.',
  failed: 'The load failed.',
};

/** Why a load was refused or failed: a reason of the load, or one of a blocked bundle. */
export function loadReasonText(reason?: string) {
  if (!reason) return '';
  return loadReasons[reason] ?? (reason in blocked ? blockedText(reason) : '');
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
