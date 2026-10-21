import assert from "node:assert/strict";
import test from "node:test";

import { gatewayModeHeadline, gatewayModeSummary } from "../src/pages/gatewayState.js";

test("saved off does not masquerade as running off before apply", () => {
  const state = { saved: "off" as const, applied: "on" as const, running: "on" as const, dirty: true };
  assert.match(gatewayModeHeadline(state.running), /透明代理运行中/);
  const summary = gatewayModeSummary(state);
  assert.match(summary, /正在运行：开启/);
  assert.match(summary, /已应用：开启/);
  assert.match(summary, /已保存目标：关闭（待应用）/);
});
