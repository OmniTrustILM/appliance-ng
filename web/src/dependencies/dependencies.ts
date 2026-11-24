import type { components } from '../api/schema';

export type Dependencies = components['schemas']['Dependencies'];
type DependenciesUpdate = components['schemas']['DependenciesUpdate'];
type DependencyTest = components['schemas']['DependencyTest'];
export type Reason = NonNullable<components['schemas']['DependencyTestResult']['reason']>;
export type Kind = 'database' | 'messaging' | 'identity';
export type Mode = components['schemas']['DependencyMode'];
export type TLSMode = NonNullable<components['schemas']['DatabaseDependency']['tlsMode']> | '';

export const kinds: Kind[] = ['database', 'messaging', 'identity'];

/** The form of one dependency; each kind uses its own fields of it. */
export type DependencyAnswers = {
  mode: Mode;
  host: string;
  port: string;
  name: string;
  virtualHost: string;
  username: string;
  issuer: string;
  clientId: string;
  /** Scopes apart by spaces. */
  scopes: string;
  /** Accepted audiences apart by spaces. */
  audiences: string;
  /** The password or client secret; write-only, empty keeps the one the appliance holds. */
  secret: string;
  /** Whether the appliance holds a password or client secret. */
  secretSet: boolean;
  /** The database's sslmode; empty is the drivers' default. */
  tlsMode: TLSMode;
  /** Whether the broker is reached with AMQPS. */
  tls: boolean;
  /** A new PEM CA; empty keeps the one the appliance holds. */
  ca: string;
  /** Whether the appliance holds a CA. */
  caSet: boolean;
};

export type DependenciesAnswers = Record<Kind, DependencyAnswers>;

const defaultPorts: Record<Kind, string> = { database: '5432', messaging: '5672', identity: '' };

const empty = (kind: Kind): DependencyAnswers => ({
  mode: 'managed', host: '', port: defaultPorts[kind], name: '', virtualHost: '', username: '', issuer: '', clientId: '',
  scopes: '', audiences: '', secret: '', secretSet: false, tlsMode: '', tls: false, ca: '', caSet: false,
});

export const managedDependencies: DependenciesAnswers = { database: empty('database'), messaging: empty('messaging'), identity: empty('identity') };

/** The answers for the dependencies the appliance reports; no password or secret is ever reported. */
export function dependencyAnswersOf(d: Dependencies): DependenciesAnswers {
  const { database: db, messaging: mq, identity: id } = d;
  return {
    database: {
      ...empty('database'), mode: db.mode, host: db.host ?? '', port: String(db.port ?? defaultPorts.database), name: db.name ?? '',
      username: db.username ?? '', secretSet: db.passwordSet ?? false, tlsMode: db.tlsMode ?? '', caSet: db.caSet ?? false,
    },
    messaging: {
      ...empty('messaging'), mode: mq.mode, host: mq.host ?? '', port: String(mq.port ?? defaultPorts.messaging),
      virtualHost: mq.virtualHost ?? '', username: mq.username ?? '', secretSet: mq.passwordSet ?? false, tls: mq.tls ?? false,
      caSet: mq.caSet ?? false,
    },
    identity: {
      ...empty('identity'), mode: id.mode, issuer: id.issuer ?? '', clientId: id.clientId ?? '', scopes: (id.scopes ?? []).join(' '),
      audiences: (id.audiences ?? []).join(' '), secretSet: id.clientSecretSet ?? false,
    },
  };
}

/** The words of a list typed apart by spaces or commas. */
const words = (list: string) => list.split(/[\s,]+/).filter(Boolean);

/** The request body of one dependency; an empty password or secret is left out. */
export function dependencyRequest(kind: Kind, a: DependencyAnswers) {
  if (a.mode === 'managed') return { mode: a.mode };
  const secret = a.secret || undefined;
  const ca = a.ca.trim() || undefined;
  switch (kind) {
    case 'database':
      return {
        mode: a.mode, host: a.host.trim(), port: Number(a.port), name: a.name.trim(), username: a.username, password: secret,
        tlsMode: a.tlsMode || undefined, ca: a.tlsMode === 'verify-full' ? ca : undefined,
      };
    case 'messaging':
      return {
        mode: a.mode, host: a.host.trim(), port: Number(a.port), virtualHost: a.virtualHost, username: a.username, password: secret,
        tls: a.tls || undefined, ca: a.tls ? ca : undefined,
      };
    case 'identity':
      return {
        mode: a.mode, issuer: a.issuer.trim(), clientId: a.clientId.trim(), scopes: words(a.scopes), audiences: words(a.audiences),
        clientSecret: secret,
      };
  }
}

/** The request body of every dependency. */
export function dependenciesRequest(answers: DependenciesAnswers): DependenciesUpdate {
  return {
    database: dependencyRequest('database', answers.database),
    messaging: dependencyRequest('messaging', answers.messaging),
    identity: dependencyRequest('identity', answers.identity),
  };
}

/** The request body of a connection test of one dependency. */
export function testRequest(kind: Kind, answers: DependencyAnswers): DependencyTest {
  return { kind, settings: dependencyRequest(kind, answers) as DependencyTest['settings'] };
}

/** The answers with the passwords and secrets cleared, as the appliance keeps them once they are sent. */
export function sentDependencies(answers: DependenciesAnswers): DependenciesAnswers {
  const sent = (a: DependencyAnswers): DependencyAnswers => {
    const external = a.mode === 'external';
    const verified = a.tlsMode === 'verify-full' || a.tls;
    return { ...a, secretSet: external && (a.secretSet || a.secret !== ''), secret: '', caSet: external && verified && (a.caSet || a.ca.trim() !== ''), ca: '' };
  };
  return { database: sent(answers.database), messaging: sent(answers.messaging), identity: sent(answers.identity) };
}

export function dependenciesText(answers: DependenciesAnswers): string {
  const { database: db, messaging: mq, identity: id } = answers;
  return [
    db.mode === 'external' ? `database ${db.host}:${db.port}/${db.name}` : 'managed database',
    mq.mode === 'external' ? `broker ${mq.host}:${mq.port}` : 'managed broker',
    id.mode === 'external' ? `identity provider ${id.issuer}` : 'managed Keycloak',
  ].join(', ');
}

const reasons: Record<Reason, string> = {
  unreachable: 'The server cannot be reached from the appliance.',
  timeout: 'The server did not answer in time.',
  authFailed: 'The server refused the user name or the password, or the database refused its name.',
  notFound: 'The issuer has no discovery document, or the broker refuses the user on the virtual host.',
  untrusted: "The server's certificate is not signed by the CA or does not name the host.",
  invalid: 'The server does not answer as expected: check the port, or that the issuer URL is exact.',
  failed: 'The test failed.',
};

/** With verify-full, libpq reports a server certificate that does not verify like refused credentials. */
const refusedOrUntrusted =
  "The database refused the user name, the password or the database name, or its certificate is not signed by the CA or does not name the host.";

/** The text of a fixed reason a test failed with; verifying is a database test under verify-full. */
export function reasonText(reason: Reason, verifying = false) {
  return reason === 'authFailed' && verifying ? refusedOrUntrusted : reasons[reason];
}

/** The errors of a test, keyed as the fields of the dependency: settings.host becomes dependencies.database.host. */
export function testFieldOf(kind: Kind, field: string) {
  return field.startsWith('settings.') ? `dependencies.${kind}.${field.slice('settings.'.length)}` : field;
}
