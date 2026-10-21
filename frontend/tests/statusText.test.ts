import assert from "node:assert/strict";
import test from "node:test";
import {
  capabilityStatusLabel,
  cutoverStageLabel,
  drainErrorLabel,
  cutoverResultLabel,
  failoverKindLabel,
  failoverReasonLabel,
  failoverSourceLabel,
  failoverStatusLabel,
  resultStatusLabel,
  rollbackStageLabel,
  runtimePhaseLabel,
  serviceStatusLabel,
  snapshotKindLabel,
  snapshotResultStatusLabel,
  workerEventKindLabel,
} from "../src/lib/statusText.js";

test("localizes cutover stages and drain failures", () => {
  assert.equal(cutoverStageLabel("verify_new_path", "verifying"), "出口验证");
  assert.equal(cutoverStageLabel("future_stage", "retrying"), "重试清理中");
  assert.equal(cutoverStageLabel("persist_committed", "failed"), "保存切换结果");
  assert.equal(cutoverStageLabel("stop_unbound_worker", "failed"), "停止旧 Worker");
  assert.equal(drainErrorLabel("worker stop: context deadline exceeded"), "停止旧 Worker 失败");
  assert.equal(drainErrorLabel("persist drain: read-only file system"), "排空状态写盘失败");
});

test("localizes worker event kinds without exposing unknown values", () => {
  assert.equal(workerEventKindLabel("cutover_ready"), "切换路径就绪");
  assert.equal(workerEventKindLabel("route_prepare_failed"), "路由准备失败");
  assert.equal(workerEventKindLabel("future_event"), "状态更新");
});

test("localizes failover event machine fields", () => {
  assert.equal(failoverKindLabel("fail_closed"), "故障关闭");
  assert.equal(failoverReasonLabel("cooldown_active"), "仍在切换冷却期");
  assert.equal(failoverStatusLabel("applied"), "已应用");
  assert.equal(cutoverResultLabel("cutover_complete"), "切换完成");
  assert.equal(failoverSourceLabel("worker_supervisor"), "Worker 监督");
  assert.equal(failoverReasonLabel("future_reason"), "其他原因");
});

test("localizes runtime and service statuses", () => {
  assert.equal(runtimePhaseLabel("running"), "运行中");
  assert.equal(runtimePhaseLabel("rescue"), "救援模式");
  assert.equal(serviceStatusLabel("stub"), "未接入");
  assert.equal(serviceStatusLabel("not_running"), "已停止或异常");
});

test("localizes snapshot kinds and result statuses", () => {
  assert.equal(snapshotKindLabel("manual"), "手动");
  assert.equal(snapshotKindLabel("scheduled"), "定时");
  assert.equal(snapshotKindLabel("before-edit"), "编辑前");
  assert.equal(resultStatusLabel("applied"), "已应用");
  assert.equal(snapshotResultStatusLabel("ok"), "已恢复");
});

test("localizes rollback stages and capability statuses", () => {
  assert.equal(rollbackStageLabel("transaction_apply"), "应用事务");
  assert.equal(rollbackStageLabel("verify_new_path"), "验证新出口");
  assert.equal(capabilityStatusLabel("supported"), "支持");
  assert.equal(capabilityStatusLabel("unsupported_by_core"), "核心不支持");
});
