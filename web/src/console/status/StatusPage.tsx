import { Link } from 'react-router';
import { SignOutButton } from '../../signin/SignOutButton';
import { ConditionList } from './ConditionList';
import { useStatus } from './useStatus';

/** The console's start page: the state of the appliance, refreshed every five seconds. */
export function StatusPage() {
  const { status, error } = useStatus();
  const complete = status?.setup === 'complete';

  return (
    <>
      <h1>Appliance</h1>
      <SignOutButton />
      {error && <p className="form-error" role="alert">{error}</p>}
      {status && (
        <>
          <p>{complete ? 'Setup is complete.' : 'Setup is not complete.'}</p>
          <p>
            {complete
              ? <Link to="/wizard/done">ILM address and administrator credential</Link>
              : <Link to="/wizard/">Continue the setup</Link>}
          </p>
          {status.rebootRequired && <p className="form-warning">The node needs to restart to apply a change.</p>}
          {status.addonStep && <p>Installing add-on: {status.addonStep}</p>}
          <h2>Conditions</h2>
          <ConditionList conditions={status.conditions} />
          <h2>Versions</h2>
          <dl>
            {Object.entries(status.versions).map(([name, version]) => (
              <div key={name}>
                <dt>{name}</dt>
                <dd>{version}</dd>
              </div>
            ))}
          </dl>
        </>
      )}
    </>
  );
}
