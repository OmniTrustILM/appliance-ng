import { useState } from 'react';
import type { components } from '../../api/schema';

export type Target = components['schemas']['BackupTarget'];
export type Update = components['schemas']['BackupTargetUpdate'];
export type Kind = Target['type'];
export type NFSVersion = NonNullable<components['schemas']['NFSSettings']['version']>;

export const defaultSchedule = '0 2 * * *';
const defaultRetention = '14';

/** The browser's IANA time zone, which a new target's schedule runs in. */
export function browserTimeZone() {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

export type Values = {
  name: string;
  type: Kind;
  schedule: string;
  timeZone: string;
  retention: string;
  endpoint: string;
  bucket: string;
  prefix: string;
  region: string;
  accessKey: string;
  secretKey: string;
  ca: string;
  removeCa: boolean;
  host: string;
  port: string;
  path: string;
  user: string;
  hostKey: string;
  password: string;
  privateKey: string;
  server: string;
  exportPath: string;
  nfsPath: string;
  version: NFSVersion;
};

/** The fields entered as text. */
export type TextKey = { [K in keyof Values]: Values[K] extends string ? K : never }[keyof Values];

/** Sets one value of a target. */
export type SetValue = <K extends keyof Values>(key: K, value: Values[K]) => void;

function initialValues(editing?: Target): Values {
  return {
    name: editing?.name ?? '',
    type: editing?.type ?? 's3',
    schedule: editing?.schedule ?? defaultSchedule,
    timeZone: editing?.timeZone ?? browserTimeZone(),
    retention: String(editing?.retention ?? defaultRetention),
    endpoint: editing?.s3?.endpoint ?? '',
    bucket: editing?.s3?.bucket ?? '',
    prefix: editing?.s3?.prefix ?? '',
    region: editing?.s3?.region ?? '',
    accessKey: '',
    secretKey: '',
    ca: '',
    removeCa: false,
    host: editing?.sftp?.host ?? '',
    port: String(editing?.sftp?.port ?? 22),
    path: editing?.sftp?.path ?? '',
    user: editing?.sftp?.user ?? '',
    hostKey: editing?.sftp?.hostKey ?? '',
    password: '',
    privateKey: '',
    server: editing?.nfs?.server ?? '',
    exportPath: editing?.nfs?.export ?? '',
    nfsPath: editing?.nfs?.path ?? '',
    version: editing?.nfs?.version ?? '4.1',
  };
}

/** The values of a target form, starting from `editing`; a changed host or port drops the accepted host key. */
export function useTargetValues(editing?: Target): [Values, SetValue] {
  const [values, setValues] = useState(() => initialValues(editing));
  const set: SetValue = (key, value) => {
    setValues((v) => {
      const next = { ...v, [key]: value };
      // A key accepted for one server is not accepted for another.
      if ((key === 'host' || key === 'port') && value !== v[key]) next.hostKey = '';
      return next;
    });
  };
  return [values, set];
}

/** The request body; secrets left blank are left out, so that the stored ones stay. */
export function updateOf(v: Values): Update {
  const blank = (s: string) => s || undefined;
  const common = { type: v.type, schedule: v.schedule.trim(), timeZone: v.timeZone.trim(), retention: Number(v.retention) };
  switch (v.type) {
    case 's3':
      return { ...common, s3: { endpoint: v.endpoint.trim(), bucket: v.bucket.trim(), prefix: blank(v.prefix), region: blank(v.region),
        accessKey: blank(v.accessKey), secretKey: blank(v.secretKey), ca: v.removeCa ? undefined : blank(v.ca),
        removeCa: v.removeCa || undefined } };
    case 'sftp':
      return { ...common, sftp: { host: v.host.trim(), port: Number(v.port), path: v.path.trim(), user: v.user, hostKey: v.hostKey,
        password: blank(v.password), privateKey: blank(v.privateKey) } };
    case 'nfs':
      return { ...common, nfs: { server: v.server.trim(), export: v.exportPath.trim(), path: blank(v.nfsPath), version: v.version } };
  }
}
