import type { components } from '../api/schema';
import { dependenciesRequest, managedDependencies, type DependenciesAnswers } from '../dependencies/dependencies';

export type MailSecurity = components['schemas']['MailSecurity'];
export type CatalogueItem = components['schemas']['CatalogueItem'];
type Appliance = components['schemas']['ApplianceSettings'];
type MailSettings = components['schemas']['MailSettings'];

export type MailAnswers = {
  host: string;
  port: string;
  security: MailSecurity;
  from: string;
  username: string;
  /** Write-only: empty keeps the password the appliance holds. */
  password: string;
};

export type Answers = {
  hostName: string;
  components: string[];
  dependencies: DependenciesAnswers;
  utils: boolean;
  mail?: MailAnswers;
};

export type Step = 'host' | 'certificate' | 'components' | 'dependencies' | 'mail' | 'review' | 'progress';

export type Errors = Partial<Record<string, string>>;

const credentialsNeedTls = 'Credentials need security starttls or tls; choose one, or remove the user name.';

export const emptyMail: MailAnswers = { host: '', port: '25', security: 'none', from: '', username: '', password: '' };

/** The mail answers for the settings the appliance holds; the password is never reported. */
export function mailAnswersOf({ host, port, security, from, username }: MailSettings): MailAnswers {
  return { host, port: String(port), security, from, username: username ?? '', password: '' };
}

/** The request body of the mail server; an empty user name or password is left out. */
export function mailRequest({ host, port, security, from, username, password }: MailAnswers): components['schemas']['Mail'] {
  return { host, port: Number(port), security, from, ...(username && { username }), ...(password && { password }) };
}

/** The answers the appliance holds; the host name defaults to the node's, the dependencies to managed ones. */
export function answersOf(appliance: Appliance, dependencies: DependenciesAnswers = managedDependencies, utils = false): Answers {
  const { mail, platform, cluster } = appliance;
  return {
    hostName: platform?.hostName ?? cluster.nodes[0]?.hostName ?? '',
    components: platform?.components ?? [],
    dependencies,
    utils,
    mail: mail && mailAnswersOf(mail),
  };
}

/** The request body; an empty user name, password or secret is left out. */
export function toRequest({ hostName, components, dependencies, utils, mail }: Answers): components['schemas']['PlatformSettings'] {
  const body = { hostName, components, dependencies: dependenciesRequest(dependencies), utils };
  if (!mail) return body;
  return { ...body, mail: mailRequest(mail) };
}

/** Whether the appliance keeps the password it holds for mail: only for the user name and server it was given for. */
export function keepsStoredPassword(stored: MailAnswers | undefined, mail: MailAnswers | undefined): boolean {
  return !!stored && !!mail && stored.username === mail.username && stored.security === mail.security &&
    stored.host.trim().toLowerCase() === mail.host.trim().toLowerCase() && Number(stored.port) === Number(mail.port);
}

/** The step that asks for a request field. */
export function stepOfField(field: string): Step {
  if (field === 'hostName') return 'host';
  if (field === 'components') return 'components';
  if (field.startsWith('dependencies.')) return 'dependencies';
  return 'mail';
}

/** Problems of the mail fields, keyed by the request field name. */
export function mailProblems({ host, port, from, security, username }: MailAnswers): Errors {
  const problems: Errors = {};
  if (!host.trim()) problems['mail.host'] = 'The mail server is required.';
  const number = Number(port);
  if (!Number.isInteger(number) || number < 1 || number > 65535) problems['mail.port'] = 'The port is a number between 1 and 65535.';
  if (!from.trim()) problems['mail.from'] = 'The from address is required.';
  if (username && security === 'none') problems['mail.username'] = credentialsNeedTls;
  return problems;
}
