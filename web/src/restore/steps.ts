import type { components } from '../api/schema';

export type Progress = components['schemas']['RestoreProgress'];
type Step = Progress['step'];

const labels: Record<Exclude<Step, 'done' | 'waitingForKit'>, string> = {
  secrets: 'Write the appliance secrets',
  appliance: 'Restore the appliance settings',
  platform: 'Bring the platform up',
  stop: 'Stop ILM',
  ilmSecrets: "Write ILM's secrets",
  database: 'Load the database',
  cbom: 'Write the stored CBOMs',
  start: 'Start ILM',
  keycloak: 'Move sign-in to the ILM host name',
  certificate: 'Issue the certificate',
};

const fresh: Step[] = ['secrets', 'appliance', 'platform', 'stop', 'ilmSecrets', 'database', 'cbom', 'start', 'keycloak', 'certificate'];
const inPlace: Step[] = ['secrets', 'appliance', 'stop', 'ilmSecrets', 'database', 'cbom', 'start'];

/** The steps of a restore, in order, with their labels. */
export function stepsOf(progress: Progress) {
  return (progress.inPlace ? inPlace : fresh).map((step) => ({ step, label: labels[step as keyof typeof labels] }));
}

/** The step a restore is at: the one it resumes at while it waits for its kit. */
export function currentStep(progress: Progress): string {
  return progress.step === 'waitingForKit' ? progress.pausedStep ?? '' : progress.step;
}

/** The label of a step, or the step itself for one the console does not know. */
export function stepLabel(step: string) {
  return labels[step as keyof typeof labels] ?? step;
}

/** Whether a restore runs on its own, so that its progress is worth reading again. */
export function isRunning(progress: Progress | undefined) {
  return progress !== undefined && !progress.message && progress.step !== 'done' && progress.step !== 'waitingForKit';
}
