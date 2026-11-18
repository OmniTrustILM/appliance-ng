import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Alert } from './Alert';
import { Badge } from './Badge';
import { Button } from './Button';
import { Card } from './Card';
import { ChoiceField } from './ChoiceField';
import { cx } from './cx';
import { Field } from './Field';
import { Layout } from './Layout';
import { PageHeading } from './PageHeading';
import { Loading } from './Spinner';
import { StepIndicator } from './StepIndicator';

describe('Layout', () => {
  it('shows the product name and the page in the setup shell', () => {
    render(<Layout variant="setup"><p>page</p></Layout>);
    expect(screen.getByRole('banner')).toHaveTextContent('Appliance setup');
    expect(within(screen.getByRole('banner')).getByAltText('ILM')).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveTextContent('page');
  });

  it('skip link targets main', () => {
    render(<Layout variant="setup"><p>page</p></Layout>);
    const link = screen.getByRole('link', { name: 'Skip to content' });
    expect(link).toHaveAttribute('href', '#main');
    expect(screen.getByRole('main')).toHaveAttribute('id', 'main');
    expect(screen.getAllByRole('link')[0]).toBe(link);
  });

  it('has no banner but the footer and the theme toggle in the auth shell', () => {
    render(<Layout variant="auth"><p>page</p></Layout>);
    expect(screen.queryByRole('banner')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Theme:/ })).toBeInTheDocument();
    expect(screen.getByText(/ILM Appliance ·/)).toBeInTheDocument();
    expect(screen.getAllByAltText('OmniTrust')[0]).toBeInTheDocument();
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

  it('describes the input by hint and error', () => {
    render(<Field label="NTP servers" name="ntp" hint="Appliance time: 12:00 UTC." error="Too short." />);
    expect(screen.getByLabelText('NTP servers')).toHaveAccessibleDescription('Appliance time: 12:00 UTC. Too short.');
  });

  it('renders the code variant as a text input', () => {
    render(<Field label="Code" name="code" variant="code" />);
    expect(screen.getByLabelText('Code')).not.toHaveAttribute('aria-describedby');
  });
});

describe('ChoiceField', () => {
  it('choice field is labelled by its title', () => {
    render(<ChoiceField type="checkbox" label="ACME" description="Automatic enrolment" badge="New" />);
    expect(screen.getByRole('checkbox', { name: /ACME/ })).toBeInTheDocument();
  });

  it('disabled choice is disabled', () => {
    render(<ChoiceField type="radio" label="SCEP" disabled />);
    expect(screen.getByRole('radio', { name: /SCEP/ })).toBeDisabled();
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

  it('shows the compact step', () => {
    render(<StepIndicator steps={['Code', 'Administrator', 'Network']} current={1} />);
    expect(screen.getByText('Step 2 of 3')).toBeInTheDocument();
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '2');
  });

  it('renders no compact row when the step is unknown', () => {
    render(<StepIndicator steps={['Code', 'Administrator', 'Network']} current={-1} />);
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
    expect(screen.queryByText(/Step 0/)).not.toBeInTheDocument();
    for (const item of screen.getAllByRole('listitem')) expect(item).not.toHaveAttribute('aria-current');
  });
});

describe('Card', () => {
  it('shows a footer only with actions', () => {
    const { container, rerender } = render(<Card>body</Card>);
    expect(container.querySelector('footer')).toBeNull();
    rerender(<Card actions={<button>Next</button>}>body</Card>);
    expect(container.querySelector('footer')).not.toBeNull();
    expect(screen.getByRole('button', { name: 'Next' })).toBeInTheDocument();
  });
});

describe('PageHeading', () => {
  it('is the h1', () => {
    render(<PageHeading eyebrow="Setup" title="Enter the setup code" lead="Lead text" />);
    expect(screen.getByRole('heading', { level: 1, name: 'Enter the setup code' })).toBeInTheDocument();
    expect(screen.getByText('Setup')).toBeInTheDocument();
    expect(screen.getByText('Lead text')).toBeInTheDocument();
  });
});

describe('cx', () => {
  it('joins truthy parts', () => {
    expect(cx('a', false, null, undefined, 'b')).toBe('a b');
  });
});

describe('Button', () => {
  it('defaults to type button', () => {
    render(<Button>Save</Button>);
    expect(screen.getByRole('button', { name: 'Save' })).toHaveAttribute('type', 'button');
  });

  it('busy button is disabled and keeps its name', async () => {
    const onClick = vi.fn();
    render(<Button busy onClick={onClick}>Apply</Button>);
    const button = screen.getByRole('button', { name: 'Apply' });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('aria-busy', 'true');
    await userEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
  });

  it('busy on-brand button keeps its name', () => {
    render(<Button variant="on-brand" busy>Sign out</Button>);
    expect(screen.getByRole('button', { name: 'Sign out' })).toHaveAttribute('aria-busy', 'true');
  });
});

describe('Alert', () => {
  it('error alert is an alert, others are status', () => {
    const { rerender } = render(<Alert tone="error">Broken</Alert>);
    expect(screen.getByRole('alert')).toHaveTextContent('Broken');
    rerender(<Alert tone="info">Note</Alert>);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('Note');
  });

  it('shows its title', () => {
    render(<Alert tone="warning" title="Careful">Details</Alert>);
    expect(screen.getByRole('status')).toHaveTextContent('Careful');
  });
});

describe('Badge', () => {
  it('renders its text', () => {
    render(<Badge tone="ok">Ready</Badge>);
    expect(screen.getByText('Ready')).toBeInTheDocument();
  });
});

describe('Loading', () => {
  it('is a status with Loading…', () => {
    render(<Loading />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading…');
  });
});
