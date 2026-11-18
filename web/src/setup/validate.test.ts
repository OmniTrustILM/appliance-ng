import { describe, expect, it } from 'vitest';
import { adminProblems, isFqdn, isIpv4, isIpv4Cidr, isIpAddress, normaliseCode, splitList } from './validate';

describe('normaliseCode', () => {
  it('upper-cases, drops separators and groups by five', () => {
    expect(normaliseCode('abcde fghjk')).toBe('ABCDE-FGHJK');
    expect(normaliseCode('abc')).toBe('ABC');
    expect(normaliseCode('abcdef')).toBe('ABCDE-F');
    expect(normaliseCode('ABCDE-FGHJK-LMN')).toBe('ABCDE-FGHJK');
  });
});

describe('splitList', () => {
  it('splits on commas, spaces and new lines and drops empty items', () => {
    expect(splitList(' 1.1.1.1, 8.8.8.8\n9.9.9.9 ,')).toEqual(['1.1.1.1', '8.8.8.8', '9.9.9.9']);
    expect(splitList('')).toEqual([]);
  });
});

describe('address checks', () => {
  it('accepts a fully qualified host name only', () => {
    expect(isFqdn('node1.example.com')).toBe(true);
    expect(isFqdn('Node-1.Example.COM')).toBe(true);
    expect(isFqdn('node1')).toBe(false);
    expect(isFqdn('10.0.2.15')).toBe(false);
    expect(isFqdn('-a.example.com')).toBe(false);
    expect(isFqdn('a_b.example.com')).toBe(false);
    expect(isFqdn('a..example.com')).toBe(false);
    expect(isFqdn(`${'a'.repeat(64)}.example.com`)).toBe(false);
  });

  it('accepts dotted IPv4 addresses', () => {
    expect(isIpv4('10.0.0.5')).toBe(true);
    expect(isIpv4('10.0.0.256')).toBe(false);
    expect(isIpv4('10.0.0')).toBe(false);
    expect(isIpv4('10.0.0.05')).toBe(false);
  });

  it('accepts an IPv4 address with a prefix of 8 to 30', () => {
    expect(isIpv4Cidr('192.168.1.10/24')).toBe(true);
    expect(isIpv4Cidr('192.168.1.10/31')).toBe(false);
    expect(isIpv4Cidr('192.168.1.10/7')).toBe(false);
    expect(isIpv4Cidr('192.168.1.10')).toBe(false);
  });

  it('accepts IPv4 and IPv6 addresses for resolvers', () => {
    expect(isIpAddress('9.9.9.9')).toBe(true);
    expect(isIpAddress('2620:fe::fe')).toBe(true);
    expect(isIpAddress('dns.example.com')).toBe(false);
  });
});

describe('adminProblems', () => {
  const ok = { user: 'admin', password: 'correct horse battery', confirmation: 'correct horse battery' };

  it('accepts a valid account', () => {
    expect(adminProblems(ok)).toEqual({});
  });

  it('mirrors the server rules', () => {
    expect(adminProblems({ ...ok, user: '' }).user).toBeDefined();
    expect(adminProblems({ ...ok, user: '-admin' }).user).toBeDefined();
    expect(adminProblems({ ...ok, password: 'short', confirmation: 'short' }).password).toMatch(/12/);
    expect(adminProblems({ ...ok, password: 'my-ADMIN-password', confirmation: 'my-ADMIN-password' }).password).toMatch(/user name/);
    expect(adminProblems({ ...ok, confirmation: 'other' }).confirmation).toMatch(/match/);
  });
});
