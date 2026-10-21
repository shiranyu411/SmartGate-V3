import assert from "node:assert/strict";
import test from "node:test";
import { errorDiagnostic, errorText } from "../src/lib/errorText.js";

function apiError(message: string, code: string, apiCode?: string): Error {
  return Object.assign(new Error(message), { status: 400, code, apiCode });
}

test("uses the structured API code before the broad frontend category", () => {
  const error = apiError("wireless scan failed", "server", "wifi_scan_failed");
  assert.equal(errorText(error), "无线网络扫描失败");
  assert.equal(errorDiagnostic(error), "[wifi_scan_failed] wireless scan failed");
});

test("localizes common API codes", () => {
  assert.equal(
    errorText(apiError("revision conflict", "conflict", "conflict")),
    "数据已发生变化，请刷新后重试",
  );
  assert.equal(
    errorText(apiError("storage full", "insufficient_storage", "insufficient_storage")),
    "设备存储空间不足",
  );
});

test("specializes authentication errors on the login form", () => {
  assert.equal(
    errorText(apiError("invalid password", "unauthorized", "unauthorized"), { context: "login" }),
    "密码错误",
  );
  assert.equal(
    errorText(apiError("too many attempts", "unknown", "rate_limited"), { context: "login" }),
    "尝试次数过多，请稍后再试",
  );
});

test("keeps actionable Chinese guidance added to an API error", () => {
  const guidance = "操作被锁定；存在未完成事务，可调用 POST /api/runtime/recover 恢复";
  assert.equal(errorText(apiError(guidance, "locked", "locked")), guidance);
});

test("falls back without exposing unknown English API messages", () => {
  assert.equal(errorText(apiError("teapot", "unexpected", "unknown_code")), "操作失败，请稍后重试");
  assert.equal(errorText(new Error("boom"), { fallback: "加载失败" }), "加载失败");
  assert.equal(errorText(new Error("本地校验失败")), "本地校验失败");
});
