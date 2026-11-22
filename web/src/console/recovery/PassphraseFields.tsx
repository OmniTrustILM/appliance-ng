import { Field } from '../../components/Field';

type PassphraseFieldsProps = Readonly<{
  label: string;
  name: string;
  value: string;
  repeat: string;
  onChange: (value: string) => void;
  onRepeatChange: (value: string) => void;
  error?: string;
}>;

/** Whether a passphrase and its repetition are filled in and agree. */
export function passphrasesAgree(value: string, repeat: string) {
  return value !== '' && value === repeat;
}

/** A new passphrase and its repetition; a repetition that differs shows at its field. */
export function PassphraseFields({ label, name, value, repeat, onChange, onRepeatChange, error }: PassphraseFieldsProps) {
  const differs = repeat !== '' && value !== repeat;
  return (
    <>
      <Field label={label} name={name} type="password" autoComplete="new-password" value={value}
        onChange={(event) => onChange(event.target.value)} error={error}
        hint="At least 12 characters. Keep it apart from the kit: anyone with both controls the appliance." />
      <Field label={`Repeat the ${label.toLowerCase()}`} name={`${name}-repeat`} type="password" autoComplete="new-password"
        value={repeat} onChange={(event) => onRepeatChange(event.target.value)}
        error={differs ? 'The passphrases differ.' : undefined} />
    </>
  );
}
