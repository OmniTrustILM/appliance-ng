import type { components } from '../api/schema';

export type MailSecurity = components['schemas']['MailSecurity'];
export type CatalogueItem = components['schemas']['CatalogueItem'];
type Appliance = components['schemas']['ApplianceSettings'];

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
  mail?: MailAnswers;
};

export type Step = 'host' | 'certificate' | 'components' | 'mail' | 'review' | 'progress';

export type Errors = Partial<Record<string, string>>;

export const emptyMail: MailAnswers = { host: '', port: '25', security: 'none', from: '', username: '', password: '' };

/** The answers the appliance holds; the host name defaults to the node's. */
export function answersOf(appliance: Appliance): Answers {
  const { mail, platform, node } = appliance;
  return {
    hostName: platform?.hostName ?? node.hostName,
    components: platform?.components ?? [],
    mail: mail && {
      host: mail.host,
      port: String(mail.port),
      security: mail.security,
      from: mail.from,
      username: mail.username ?? '',
      password: '',
    },
  };
}

/** The request body; an empty user name or password is left out. */
export function toRequest({ hostName, components, mail }: Answers): components['schemas']['PlatformSettings'] {
  if (!mail) return { hostName, components };
  const { host, port, security, from, username, password } = mail;
  return {
    hostName,
    components,
    mail: { host, port: Number(port), security, from, ...(username && { username }), ...(password && { password }) },
  };
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
  return 'mail';
}

/** Problems of the mail fields, keyed by the request field name. */
export function mailProblems({ host, port, from }: MailAnswers): Errors {
  const problems: Errors = {};
  if (!host.trim()) problems['mail.host'] = 'The mail server is required.';
  const number = Number(port);
  if (!Number.isInteger(number) || number < 1 || number > 65535) problems['mail.port'] = 'The port is a number between 1 and 65535.';
  if (!from.trim()) problems['mail.from'] = 'The from address is required.';
  return problems;
}
