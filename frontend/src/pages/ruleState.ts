import type { RouteRule, RuleConflict, RuleKind } from "../lib/types.js";

export function normalizeRuleValue(kind: RuleKind, value: string): string {
  let normalized = value.trim();
  if (kind === "domain_suffix" || kind === "domain_keyword") {
    normalized = normalized.toLowerCase().replace(/^\./, "").replace(/\.$/, "");
  } else if (kind === "ip_cidr") {
    normalized = normalizeCIDR(normalized);
  } else if (kind === "geosite" || kind === "geoip") {
    normalized = normalized.toLowerCase();
  }
  return normalized;
}

function normalizeCIDR(value: string): string {
  const separator = value.lastIndexOf("/");
  if (separator <= 0) {
    const parsedIPv4 = parseIPv4(value);
    if (parsedIPv4 !== undefined) return `${formatIPv4(parsedIPv4)}/32`;
    const parsedIPv6 = parseIPv6(value);
    if (parsedIPv6 !== undefined) return `${formatIPv6(parsedIPv6)}/128`;
    return value;
  }
  if (separator === value.length - 1) return value;

  const address = value.slice(0, separator);
  const prefixText = value.slice(separator + 1);
  if (!/^\d+$/.test(prefixText)) return value;

  const prefix = Number(prefixText);
  const parsedIPv4 = parseIPv4(address);
  if (parsedIPv4 !== undefined && prefix <= 32) {
    const hostBits = BigInt(32 - prefix);
    const masked = hostBits === 0n ? parsedIPv4 : (parsedIPv4 >> hostBits) << hostBits;
    return `${formatIPv4(masked)}/${prefix}`;
  }

  const parsedIPv6 = parseIPv6(address);
  if (parsedIPv6 !== undefined && prefix <= 128) {
    const mappedIPv4 = extractMappedIPv4(parsedIPv6);
    if (mappedIPv4 !== undefined && prefix >= 96) {
      return `${formatIPv4(mappedIPv4)}/${prefix - 96}`;
    }
    const hostBits = BigInt(128 - prefix);
    const masked = hostBits === 0n ? parsedIPv6 : (parsedIPv6 >> hostBits) << hostBits;
    return `${formatIPv6(masked)}/${prefix}`;
  }

  return value;
}

function parseIPv4(value: string): bigint | undefined {
  const parts = value.split(".");
  if (parts.length !== 4) return undefined;

  let address = 0n;
  for (const part of parts) {
    if (!/^(0|[1-9]\d{0,2})$/.test(part)) return undefined;
    const octet = Number(part);
    if (octet > 255) return undefined;
    address = (address << 8n) | BigInt(octet);
  }
  return address;
}

function formatIPv4(value: bigint): string {
  return [24n, 16n, 8n, 0n]
    .map((shift) => Number((value >> shift) & 0xffn).toString())
    .join(".");
}

function extractMappedIPv4(value: bigint): bigint | undefined {
  return value >> 32n === 0xffffn ? value & 0xffffffffn : undefined;
}

function parseIPv6(value: string): bigint | undefined {
  if (value.includes("%") || value.split("::").length > 2) return undefined;

  const halves = value.split("::");
  const left = parseIPv6Groups(halves[0] ?? "");
  const right = parseIPv6Groups(halves[1] ?? "");
  if (!left || !right) return undefined;

  const hasCompression = halves.length === 2;
  const missing = 8 - left.length - right.length;
  if ((!hasCompression && missing !== 0) || (hasCompression && missing < 1)) return undefined;

  const groups = [...left, ...Array<number>(missing).fill(0), ...right];
  let address = 0n;
  for (const group of groups) {
    address = (address << 16n) | BigInt(group);
  }
  return address;
}

function parseIPv6Groups(value: string): number[] | undefined {
  if (value === "") return [];

  const parts = value.split(":");
  const groups: number[] = [];
  for (const [index, part] of parts.entries()) {
    if (part.includes(".")) {
      if (index !== parts.length - 1) return undefined;
      const ipv4 = parseIPv4(part);
      if (ipv4 === undefined) return undefined;
      groups.push(Number((ipv4 >> 16n) & 0xffffn), Number(ipv4 & 0xffffn));
      continue;
    }
    if (!/^[0-9a-fA-F]{1,4}$/.test(part)) return undefined;
    groups.push(Number.parseInt(part, 16));
  }
  return groups;
}

function formatIPv6(value: bigint): string {
  const groups = Array.from({ length: 8 }, (_, index) =>
    Number((value >> BigInt((7 - index) * 16)) & 0xffffn),
  );
  let bestStart = -1;
  let bestLength = 0;
  for (let start = 0; start < groups.length; ) {
    if (groups[start] !== 0) {
      start += 1;
      continue;
    }
    let end = start;
    while (end < groups.length && groups[end] === 0) end += 1;
    if (end - start > bestLength) {
      bestStart = start;
      bestLength = end - start;
    }
    start = end;
  }

  if (bestLength < 2) return groups.map((group) => group.toString(16)).join(":");

  const left = groups.slice(0, bestStart).map((group) => group.toString(16)).join(":");
  const right = groups
    .slice(bestStart + bestLength)
    .map((group) => group.toString(16))
    .join(":");
  return `${left}::${right}`;
}

export function ruleKey(kind: RuleKind, value: string): string {
  return `${kind}\u0000${normalizeRuleValue(kind, value)}`;
}

export function detectRuleConflicts(rules: RouteRule[]): RuleConflict[] {
  const byKey = new Map<string, { kind: RuleKind; value: string; pools: Set<string>; ids: Set<string> }>();
  for (const rule of rules) {
    if (rule.source !== "user" || !rule.enabled) continue;
    const key = ruleKey(rule.kind, rule.value);
    const current = byKey.get(key) ?? {
      kind: rule.kind,
      value: normalizeRuleValue(rule.kind, rule.value),
      pools: new Set<string>(),
      ids: new Set<string>(),
    };
    current.pools.add(rule.pool_id);
    current.ids.add(rule.id);
    byKey.set(key, current);
  }
  return [...byKey.entries()]
    .filter(([, item]) => item.pools.size > 1)
    .map(([key, item]) => ({
      rule_key: key,
      kind: item.kind,
      value: item.value,
      pools: [...item.pools].sort(),
      rule_ids: [...item.ids].sort(),
    }))
    .sort((left, right) => left.rule_key.localeCompare(right.rule_key));
}

export function removeConflictPool(
  rules: RouteRule[],
  conflict: RuleConflict,
  pool: string,
): RouteRule[] {
  return rules.filter(
    (rule) =>
      !(
        rule.source === "user" &&
        rule.enabled &&
        rule.pool_id === pool &&
        ruleKey(rule.kind, rule.value) === conflict.rule_key
      ),
  );
}

export function moveConflictPool(
  rules: RouteRule[],
  conflict: RuleConflict,
  fromPool: string,
  toPool: string,
): RouteRule[] {
  return rules.map((rule) =>
    rule.source === "user" &&
    rule.enabled &&
    rule.pool_id === fromPool &&
    ruleKey(rule.kind, rule.value) === conflict.rule_key
      ? { ...rule, pool_id: toPool }
      : rule,
  );
}

export function copyRuleAsUser(rule: RouteRule, id: string): RouteRule {
  return {
    id,
    source: "user",
    kind: rule.kind,
    value: rule.value,
    pool_id: rule.pool_id,
    enabled: true,
  };
}
