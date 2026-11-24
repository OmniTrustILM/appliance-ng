import type { components } from '../../api/schema';
import { ChoiceField } from '../../components/ChoiceField';
import { Field } from '../../components/Field';
import { TextArea } from '../../components/TextArea';

type CBOM = components['schemas']['CBOMRepository'];
type Storage = CBOM['storage'];

/** The form values of the CBOM repository; secrets are never filled from the appliance. */
export type CBOMAnswers = {
  enabled: boolean;
  storage: Storage;
  endpoint: string;
  bucket: string;
  region: string;
  accessKey: string;
  secretKey: string;
  ca: string;
  removeCa: boolean;
  accessKeySet: boolean;
  secretKeySet: boolean;
  caSet: boolean;
};

/** The form values of the repository as the appliance describes it. */
export function cbomAnswersOf({ enabled, storage, s3 }: CBOM): CBOMAnswers {
  return {
    enabled, storage, endpoint: s3?.endpoint ?? '', bucket: s3?.bucket ?? '', region: s3?.region ?? '', accessKey: '', secretKey: '', ca: '',
    removeCa: false, accessKeySet: s3?.accessKeySet ?? false, secretKeySet: s3?.secretKeySet ?? false, caSet: s3?.caSet ?? false,
  };
}

/** The request body of the repository: the bucket only for an external store, and only the secrets entered. */
export function cbomRequest(a: CBOMAnswers): CBOM {
  if (a.storage !== 's3') return { enabled: a.enabled, storage: a.storage };
  const secret = (value: string) => (value ? value : undefined);
  return {
    enabled: a.enabled,
    storage: 's3',
    s3: {
      endpoint: a.endpoint.trim(), bucket: a.bucket.trim(), region: a.region.trim(), accessKey: secret(a.accessKey), secretKey: secret(a.secretKey),
      ca: a.removeCa ? undefined : secret(a.ca), removeCa: a.removeCa || undefined,
    },
  };
}

const storages: ReadonlyArray<{ value: Storage; label: string; description: string }> = [
  { value: 'appliance', label: 'On the appliance', description: "An S3 store on the appliance's data volume. Backups include the stored CBOMs." },
  { value: 's3', label: 'External S3', description: 'A bucket of an S3 compatible store you run and back up.' },
];

const keepHint = 'Leave blank to keep the stored value while the endpoint stays the same.';

type CBOMFieldsProps = Readonly<{
  answers: CBOMAnswers;
  errors: Partial<Record<string, string>>;
  onChange: (answers: CBOMAnswers) => void;
}>;

/** Turns the CBOM repository on or off and chooses where it stores the CBOMs. */
export function CBOMFields({ answers, errors, onChange }: CBOMFieldsProps) {
  const edit = (patch: Partial<CBOMAnswers>) => onChange({ ...answers, ...patch });
  const text = (key: 'endpoint' | 'bucket' | 'region' | 'accessKey' | 'secretKey', label: string, extra: Partial<Parameters<typeof Field>[0]> = {}) => (
    <Field label={label} name={`cbom.${key}`} value={answers[key]} onChange={(e) => edit({ [key]: e.target.value })}
      autoComplete="off" spellCheck={false} error={errors[`cbom.s3.${key}`]} {...extra} />
  );
  return (
    <>
      <ChoiceField type="checkbox" label="CBOM repository" checked={answers.enabled} onChange={(e) => edit({ enabled: e.target.checked })}
        description="Keeps the CBOMs ILM stores. Only ILM reaches it, so ILM's settings page shows it as not reachable from the browser. Turned off, the CBOMs stored on the appliance are kept and still backed up." />
      {answers.enabled && (
        <>
          <fieldset className="mb-[18px]">
            <legend className="mb-1.5 text-sm font-medium">CBOM storage</legend>
            {storages.map((s) => (
              <ChoiceField key={s.value} type="radio" name="cbom.storage" label={s.label} description={s.description}
                checked={answers.storage === s.value} onChange={() => edit({ storage: s.value })} />
            ))}
            {errors['cbom.storage'] && <p className="text-[13px] text-danger" role="alert">{errors['cbom.storage']}</p>}
          </fieldset>
          {answers.storage === 's3' && (
            <>
              {text('endpoint', 'S3 endpoint', { hint: 'https://host[:port], such as https://s3.eu-central-1.amazonaws.com.' })}
              {text('bucket', 'Bucket')}
              {text('region', 'Region (optional)', { hint: 'us-east-1 when left blank.' })}
              {text('accessKey', 'Access key', { type: 'password', autoComplete: 'new-password', hint: answers.accessKeySet ? keepHint : undefined })}
              {text('secretKey', 'Secret key', { type: 'password', autoComplete: 'new-password', hint: answers.secretKeySet ? keepHint : undefined })}
              {!answers.removeCa && (
                <TextArea label="CA bundle (optional)" name="cbom.ca" value={answers.ca} onChange={(ca) => edit({ ca })} error={errors['cbom.s3.ca']}
                  hint={answers.caSet ? 'Leave blank to keep the stored CA bundle.' : 'The PEM CA certificates that sign the endpoint, when they are not publicly trusted.'} />
              )}
              {answers.caSet && (
                <ChoiceField type="checkbox" label="Remove the stored CA bundle" checked={answers.removeCa}
                  description="The endpoint's certificate must then be publicly trusted." onChange={(e) => edit({ removeCa: e.target.checked })} />
              )}
            </>
          )}
        </>
      )}
    </>
  );
}
