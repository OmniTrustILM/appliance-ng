import { useState, type FormEvent } from 'react';
import { Field } from '../components/Field';
import type { Answers, Errors } from './answers';

/** Asks for the name the ILM is reached by. */
export function HostPage({ answers, errors, onChange, onNext }: {
  answers: Answers;
  errors: Errors;
  onChange: (patch: Partial<Answers>) => void;
  onNext: () => void;
}) {
  const [problem, setProblem] = useState<string>();

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!answers.hostName.trim()) return setProblem('The host name is required.');
    setProblem(undefined);
    onNext();
  }

  return (
    <form onSubmit={submit} noValidate>
      <h1>ILM host name</h1>
      <p>The name that users and clients use to reach the ILM, for example ilm.example.com. It defaults to the host name of this node.</p>
      <Field
        label="ILM host name"
        name="hostName"
        value={answers.hostName}
        onChange={(e) => { setProblem(undefined); onChange({ hostName: e.target.value }); }}
        error={problem ?? errors.hostName}
      />
      <button type="submit">Continue</button>
    </form>
  );
}
