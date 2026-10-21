import assert from "node:assert/strict";
import test from "node:test";
import type { RouteRule } from "../src/lib/types.js";
import {
  copyRuleAsUser,
  detectRuleConflicts,
  moveConflictPool,
  normalizeRuleValue,
  removeConflictPool,
} from "../src/pages/ruleState.js";

const baseRules: RouteRule[] = [
  {
    id: "builtin",
    source: "builtin-v2",
    kind: "domain_suffix",
    value: "Example.COM",
    pool_id: "direct",
    enabled: true,
  },
  {
    id: "user-ai",
    source: "user",
    kind: "domain_suffix",
    value: ".example.com.",
    pool_id: "ai",
    enabled: true,
  },
  {
    id: "user-streaming",
    source: "user",
    kind: "domain_suffix",
    value: "example.com",
    pool_id: "streaming",
    enabled: true,
  },
  {
    id: "disabled",
    source: "user",
    kind: "domain_suffix",
    value: "example.com",
    pool_id: "general",
    enabled: false,
  },
];

test("detectRuleConflicts only considers enabled user ownership", () => {
  const conflicts = detectRuleConflicts(baseRules);
  assert.equal(conflicts.length, 1);
  assert.deepEqual(conflicts[0]?.pools, ["ai", "streaming"]);
  assert.deepEqual(conflicts[0]?.rule_ids, ["user-ai", "user-streaming"]);
});

test("removeConflictPool removes only the selected user owner", () => {
  const conflict = detectRuleConflicts(baseRules)[0];
  assert.ok(conflict);
  const next = removeConflictPool(baseRules, conflict, "ai");
  assert.equal(next.some((rule) => rule.id === "user-ai"), false);
  assert.equal(next.some((rule) => rule.id === "builtin"), true);
  assert.equal(detectRuleConflicts(next).length, 0);
});

test("moveConflictPool changes the selected owner without touching system rules", () => {
  const conflict = detectRuleConflicts(baseRules)[0];
  assert.ok(conflict);
  const next = moveConflictPool(baseRules, conflict, "ai", "streaming");
  assert.equal(next.find((rule) => rule.id === "user-ai")?.pool_id, "streaming");
  assert.equal(next.find((rule) => rule.id === "builtin")?.pool_id, "direct");
  assert.equal(detectRuleConflicts(next).length, 0);
});

test("copyRuleAsUser creates an editable override", () => {
  const copied = copyRuleAsUser(baseRules[0]!, "user-copy");
  assert.deepEqual(copied, {
    id: "user-copy",
    source: "user",
    kind: "domain_suffix",
    value: "Example.COM",
    pool_id: "direct",
    enabled: true,
  });
});

test("normalizeRuleValue masks IPv4 and IPv6 CIDRs like the backend", () => {
  assert.equal(normalizeRuleValue("ip_cidr", "203.0.113.42/24"), "203.0.113.0/24");
  assert.equal(normalizeRuleValue("ip_cidr", "2001:db8:1::1234/48"), "2001:db8:1::/48");
  assert.equal(normalizeRuleValue("ip_cidr", "203.0.113.42"), "203.0.113.42/32");
  assert.equal(normalizeRuleValue("ip_cidr", "2001:db8::42"), "2001:db8::42/128");
  assert.equal(normalizeRuleValue("ip_cidr", "::ffff:203.0.113.42/128"), "203.0.113.42/32");
  assert.equal(normalizeRuleValue("ip_cidr", "not-a-cidr"), "not-a-cidr");
});

test("detectRuleConflicts treats equivalent CIDRs as one rule key", () => {
  const conflicts = detectRuleConflicts([
    {
      id: "cidr-ai",
      source: "user",
      kind: "ip_cidr",
      value: "203.0.113.42/24",
      pool_id: "ai",
      enabled: true,
    },
    {
      id: "cidr-general",
      source: "user",
      kind: "ip_cidr",
      value: "203.0.113.0/24",
      pool_id: "general",
      enabled: true,
    },
  ]);

  assert.equal(conflicts.length, 1);
  assert.equal(conflicts[0]?.value, "203.0.113.0/24");
  assert.deepEqual(conflicts[0]?.pools, ["ai", "general"]);
});
