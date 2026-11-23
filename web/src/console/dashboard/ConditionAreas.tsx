import { ConditionList } from './ConditionList';
import type { Status } from './useStatus';

/** The area of the appliance each condition reports on, in display order. */
const areas = [
  { name: 'Setup', types: ['HandoverComplete'] },
  { name: 'Node', types: ['NodeConfigured'] },
  { name: 'Add-ons', types: ['AddonsReady'] },
  { name: 'Platform', types: ['PlatformReady', 'ConnectorsReady'] },
  { name: 'Certificate', types: ['CertificateReady'] },
  { name: 'Backups', types: ['BackupHealthy'] },
] as const;

const known: ReadonlySet<string> = new Set(areas.flatMap((area) => area.types));

/** The conditions grouped by area; areas without a condition are left out, unknown conditions come last. */
export function ConditionAreas({ conditions }: Readonly<{ conditions: Status['conditions'] }>) {
  const groups = [
    ...areas.map(({ name, types }) => ({ name, conditions: conditions.filter((c) => (types as readonly string[]).includes(c.type)) })),
    { name: 'Other', conditions: conditions.filter((c) => !known.has(c.type)) },
  ].filter((group) => group.conditions.length > 0);

  return (
    <div className="grid gap-4">
      {groups.map((group) => (
        <section key={group.name}>
          <h3 className="text-xs font-semibold uppercase tracking-[0.06em] text-content-subtle">{group.name}</h3>
          <ConditionList conditions={group.conditions} />
        </section>
      ))}
    </div>
  );
}
