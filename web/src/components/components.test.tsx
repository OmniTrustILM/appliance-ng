import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Field } from './Field';
import { Layout } from './Layout';
import { StepIndicator } from './StepIndicator';

describe('Layout', () => {
  it('shows the appliance name and the page', () => {
    render(<Layout><p>page</p></Layout>);
    expect(screen.getByRole('banner')).toHaveTextContent('ILM Appliance');
    expect(screen.getByRole('main')).toHaveTextContent('page');
  });
});

describe('Field', () => {
  it('labels its input', () => {
    render(<Field label="Host name" name="hostName" />);
    const input = screen.getByLabelText('Host name');
    expect(input).toHaveAttribute('name', 'hostName');
    expect(input).not.toHaveAttribute('aria-invalid');
  });

  it('shows its error and marks the input invalid', () => {
    render(<Field label="Password" name="password" type="password" error="Too short." />);
    const input = screen.getByLabelText('Password');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input).toHaveAccessibleDescription('Too short.');
    expect(screen.getByRole('alert')).toHaveTextContent('Too short.');
  });
});

describe('StepIndicator', () => {
  it('marks the current step', () => {
    render(<StepIndicator steps={['Code', 'Administrator', 'Network']} current={1} />);
    const items = screen.getAllByRole('listitem');
    expect(items).toHaveLength(3);
    expect(items[1]).toHaveAttribute('aria-current', 'step');
    expect(items[0]).toHaveClass('done');
    expect(items[2]).not.toHaveAttribute('aria-current');
  });
});
