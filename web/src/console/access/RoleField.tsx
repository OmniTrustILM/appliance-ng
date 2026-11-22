import { ChoiceField } from '../../components/ChoiceField';

export type RoleChoice<T extends string> = Readonly<{ value: T; label: string; description: string }>;

type RoleFieldProps<T extends string> = Readonly<{
  name: string;
  roles: readonly RoleChoice<T>[];
  value: T;
  onChange: (role: T) => void;
  error?: string;
}>;

/** The role of a credential to issue, one of roles. */
export function RoleField<T extends string>({ name, roles, value, onChange, error }: RoleFieldProps<T>) {
  return (
    <fieldset className="mb-[18px]">
      <legend className="mb-1.5 text-sm font-medium">Role</legend>
      {roles.map((r) => (
        <ChoiceField key={r.value} type="radio" name={name} label={r.label} description={r.description}
          checked={value === r.value} onChange={() => onChange(r.value)} />
      ))}
      {error && <p className="text-[13px] text-danger" role="alert">{error}</p>}
    </fieldset>
  );
}
