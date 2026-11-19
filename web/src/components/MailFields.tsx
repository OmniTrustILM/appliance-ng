import { Field, INPUT } from './Field';
import type { Errors, MailAnswers, MailSecurity } from '../wizard/answers';

const securities: { value: MailSecurity; label: string }[] = [
  { value: 'none', label: 'None' },
  { value: 'starttls', label: 'STARTTLS' },
  { value: 'tls', label: 'TLS' },
];

/** The fields of the mail server; errors are keyed by request field name, such as `mail.host`. */
export function MailFields({ mail, passwordSet, errors, onEdit }: Readonly<{
  mail: MailAnswers;
  passwordSet: boolean;
  errors: Errors;
  onEdit: (patch: Partial<MailAnswers>) => void;
}>) {
  return (
    <>
      <div className="grid gap-x-4 sm:grid-cols-[1fr_140px]">
        <Field label="Mail server" name="mail.host" value={mail.host} onChange={(e) => onEdit({ host: e.target.value })} error={errors['mail.host']} />
        <Field label="Port" name="mail.port" inputMode="numeric" value={mail.port} onChange={(e) => onEdit({ port: e.target.value })} error={errors['mail.port']} />
      </div>
      <div className="mb-[18px]">
        <label htmlFor="mail-security" className="mb-1.5 block text-sm font-medium">Security</label>
        <select id="mail-security" name="mail.security" className={INPUT} value={mail.security} onChange={(e) => onEdit({ security: e.target.value as MailSecurity })}>
          {securities.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
        </select>
        {errors['mail.security'] && <p className="mt-1.5 text-[13px] text-danger" role="alert">{errors['mail.security']}</p>}
      </div>
      <Field label="From address" name="mail.from" value={mail.from} onChange={(e) => onEdit({ from: e.target.value })} error={errors['mail.from']} autoComplete="off" />
      <Field label="User name (optional)" name="mail.username" value={mail.username} onChange={(e) => onEdit({ username: e.target.value })} error={errors['mail.username']} autoComplete="off" />
      <Field
        label="Password (optional)"
        name="mail.password"
        type="password"
        value={mail.password}
        onChange={(e) => onEdit({ password: e.target.value })}
        error={errors['mail.password']}
        hint={passwordSet ? 'A password is set. Leave the field empty to keep it.' : undefined}
        autoComplete="new-password"
      />
    </>
  );
}
