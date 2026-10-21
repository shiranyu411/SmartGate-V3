import assert from "node:assert/strict";
import test from "node:test";
import type { SmartNode } from "../src/lib/types.js";
import { coreLabel, coreLabelMap } from "../src/lib/coreLabels.js";
import { coreIDsForNode, filterSmartNodes, groupSmartNodesByProvider, requiresDegradedConfirmation, selectableCores } from "../src/pages/nodeState.js";

const node: SmartNode = {
  id: "sn_test",
  revision: 2,
  display_name: "Hong Kong AI",
  identity: {
    protocol: "vless",
    server: "hk.example.com",
    port: 443,
    transport: { type: "xhttp" },
    security: { type: "reality" },
    secret_ref_masked: "sha256:****beef",
  },
  aliases: [{ provider_id: "airport-a", raw_tag: "HK-AI", first_seen: "2026-01-01T00:00:00Z", last_seen: "2026-01-02T00:00:00Z" }],
  capability: {
    alpha: { status: "degraded", reasons: ["xhttp"] },
    beta: { status: "supported", reasons: [] },
  },
  core_choice: "beta",
  referenced_by_pools: ["ai"],
  health: { status: "ok", delay_ms: 80, context: { revision: 2, core: "beta", method: "executor_http_204" } },
};

test("filters SmartNodes across catalog dimensions", () => {
  const base = { keyword: "", provider: "", protocol: "", core: "", capability: "", health: "" };
  assert.equal(filterSmartNodes([node], { ...base, keyword: "hk.example" }).length, 1);
  assert.equal(filterSmartNodes([node], { ...base, keyword: node.id }).length, 1);
  assert.equal(filterSmartNodes([node], { ...base, provider: "airport-b" }).length, 0);
  assert.equal(filterSmartNodes([node], { ...base, core: "unset" }).length, 0);
  assert.equal(filterSmartNodes([node], { ...base, capability: "degraded" }).length, 1);
  assert.equal(filterSmartNodes([node], { ...base, health: "failed" }).length, 0);
});

test("groups by configured provider order without reordering subscription members", () => {
  const first = { ...node, id: "sn_z" };
  const second = { ...node, id: "sn_a" };
  const other = { ...node, id: "sn_other", aliases: [{ ...node.aliases[0], provider_id: "airport-b" }] };
  const missing = { ...node, id: "sn_missing", source_missing: true };
  const input = [first, other, second, missing];
  const groups = groupSmartNodesByProvider(input, ["airport-b", "airport-a"]);
  assert.deepEqual(groups.map((group) => group.providerID), ["airport-b", "airport-a", ""]);
  assert.deepEqual(groups[1].nodes.map((item) => item.id), ["sn_z", "sn_a"]);
  assert.deepEqual(input.map((item) => item.id), ["sn_z", "sn_other", "sn_a", "sn_missing"]);
});

test("shared nodes appear once under the first configured provider", () => {
  const shared = { ...node, aliases: [...node.aliases, { ...node.aliases[0], provider_id: "airport-b" }] };
  const groups = groupSmartNodesByProvider([shared], ["airport-b", "airport-a", "airport-b"]);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].providerID, "airport-b");
  assert.deepEqual(groups[0].nodes, [shared]);
});

test("core choices include degraded but exclude unsupported", () => {
  assert.deepEqual(selectableCores(node), ["alpha", "beta"]);
  assert.equal(requiresDegradedConfirmation(node, "alpha"), true);
  assert.equal(requiresDegradedConfirmation(node, "beta"), false);

  const unsupported = structuredClone(node);
  unsupported.capability.alpha = { status: "unsupported_by_core", reasons: ["blocked"] };
  assert.deepEqual(selectableCores(unsupported), ["beta"]);
});

test("dynamic capability keys expose additional cores", () => {
  const withThirdCore = structuredClone(node);
  const thirdCore = "third-core";
  withThirdCore.capability[thirdCore] = { status: "supported", reasons: [] };
  assert.deepEqual(coreIDsForNode(withThirdCore), [...coreIDsForNode(node), thirdCore]);
  assert.deepEqual(selectableCores(withThirdCore), [...selectableCores(node), thirdCore]);
  assert.equal(filterSmartNodes([withThirdCore], { keyword: "", provider: "", protocol: "", core: "", capability: "supported", health: "" }).length, 1);
});

test("missing capability keys do not throw", () => {
  const missing = structuredClone(node);
  const removedCore = coreIDsForNode(node)[1];
  delete missing.capability[removedCore];
  assert.doesNotThrow(() => filterSmartNodes([missing], { keyword: "", provider: "", protocol: "", core: "", capability: "supported", health: "" }));
  assert.equal(requiresDegradedConfirmation(missing, removedCore), false);
});

test("core labels fall back to raw core id", () => {
  const labels = coreLabelMap([{ id: "alpha", display_name: "Alpha Core" }]);
  assert.equal(coreLabel("alpha", labels), "Alpha Core");
  assert.equal(coreLabel("core-c", labels), "core-c");
  assert.equal(coreLabel(null, labels), "未设置");
});
