import assert from "node:assert/strict";
import test from "node:test";
import type { CutoverResult, PoolBinding, RuntimeStatus } from "../src/lib/types.js";
import {
  applyCutoverError,
  applyCutoverResult,
  applyRuntimeProgress,
  beginCutover,
  bindingSelection,
  cutoverSource,
  isCutoverBusy,
  strictProgressIndex,
} from "../src/pages/cutoverState.js";

const sourceBinding: PoolBinding = {
  id: "pb_general_1",
  pool: "general",
  target: { kind: "node", smart_node: "sn_old", core: "alpha", worker: "worker-alpha", executor: "ne_old" },
  generation: 7,
  since: "2026-07-29T00:00:00Z",
};

const targetBinding: PoolBinding = {
  ...sourceBinding,
  id: "pb_general_2",
  target: { kind: "node", smart_node: "sn_new", core: "alpha", worker: "worker-alpha", executor: "ne_new" },
  generation: 8,
};

function result(overrides: Partial<CutoverResult> = {}): CutoverResult {
  return {
    result: "cutover_complete",
    binding: targetBinding,
    verified: true,
    ...overrides,
  };
}

function runtime(overrides: Partial<RuntimeStatus> = {}): RuntimeStatus {
  return {
    phase: "running",
    listen_addr: "192.168.1.1:3000",
    config_epoch: 12,
    topology_hash: "topology",
    rescue_active: false,
    rescue_reason: "",
    workers: [],
    bindings: [sourceBinding],
    cutovers_in_flight: [],
    drains: [],
    ...overrides,
  };
}

test("strict cutover only completes on verified cutover_complete", () => {
  const pending = beginCutover("strict", "sn_new");
  const committed = applyCutoverResult(pending, result({ result: "committed" }));
  assert.equal(committed.phase, "failed");

  const complete = applyCutoverResult(pending, result({ closed_connections: 4 }));
  assert.equal(complete.phase, "complete");
  assert.equal(complete.closedConnections, 4);
  assert.equal(strictProgressIndex(complete), 3);
});

test("strict runtime stages map to the required three-step progress", () => {
  const pending = beginCutover("strict", "sn_new");
  const closing = applyRuntimeProgress(
    pending,
    runtime({ cutovers_in_flight: [{ pool: "general", stage: "close_old_binding" }] }),
    "general",
  );
  assert.equal(closing.phase, "closing");
  assert.equal(strictProgressIndex(closing), 1);

  const verifying = applyRuntimeProgress(
    closing,
    runtime({ cutovers_in_flight: [{ pool: "general", stage: "verify_new_path" }] }),
    "general",
  );
  assert.equal(verifying.phase, "verifying");
  assert.equal(strictProgressIndex(verifying), 2);
});

test("graceful cutover follows SSE drain until completion", () => {
  const pending = beginCutover("graceful", "sn_new");
  const draining = applyCutoverResult(
    pending,
    result({
      result: "committed",
      draining: { old_generation: 7, remaining: 9, deadline: "2026-07-29T00:00:30Z" },
    }),
  );
  assert.equal(draining.phase, "draining");
  assert.equal(draining.remaining, 9);
  assert.equal(isCutoverBusy(draining), true);

  const updated = applyRuntimeProgress(
    draining,
    runtime({ drains: [{ pool: "general", old_generation: 7, remaining: 2, deadline: "2026-07-29T00:00:30Z", status: "draining", retry_count: 0 }] }),
    "general",
  );
  assert.equal(updated.remaining, 2);

  const complete = applyRuntimeProgress(updated, runtime(), "general");
  assert.equal(complete.phase, "complete");
  assert.equal(complete.remaining, 0);
});

test("graceful drain keeps retrying and failed cleanup visible", () => {
  const draining = applyCutoverResult(
    beginCutover("graceful", "sn_new"),
    result({
      result: "committed",
      draining: { old_generation: 7, remaining: 3, deadline: "2026-07-29T00:00:30Z" },
    }),
  );
  const retrying = applyRuntimeProgress(
    draining,
    runtime({
      drains: [{
        pool: "general",
        old_generation: 7,
        remaining: 3,
        deadline: "2026-07-29T00:00:30Z",
        status: "retrying",
        last_error: "worker cleanup failed",
        retry_count: 2,
      }],
    }),
    "general",
  );
  assert.equal(retrying.phase, "retrying");
  assert.equal(retrying.retryCount, 2);
  assert.equal(isCutoverBusy(retrying), true);

  const failed = applyRuntimeProgress(
    retrying,
    runtime({
      drains: [{
        pool: "general",
        old_generation: 7,
        remaining: 3,
        deadline: "2026-07-29T00:00:30Z",
        status: "failed",
        last_error: "worker cleanup failed",
        retry_count: 3,
      }],
    }),
    "general",
  );
  assert.equal(failed.phase, "drain_failed");
  assert.equal(failed.lastError, "worker cleanup failed");
  assert.equal(isCutoverBusy(failed), true);

  const recovered = applyRuntimeProgress(failed, runtime(), "general");
  assert.equal(recovered.phase, "complete");
  assert.equal(isCutoverBusy(recovered), false);
  assert.match(recovered.message, /恢复完成/);
});

test("cutover errors distinguish rollback, block, conflict, and config Apply", () => {
  const pending = beginCutover("strict", "sn_new");
  const rolledBack = applyCutoverError(pending, {
    apiCode: "cutover_failed",
    message: "failed",
    details: { stage: "verify_new_path", rolled_back: true, blocked: false },
  });
  assert.equal(rolledBack.phase, "failed");
  assert.equal(rolledBack.rolledBack, true);
  assert.match(rolledBack.message, /已回滚/);

  const blocked = applyCutoverError(pending, {
    apiCode: "cutover_failed",
    message: "failed",
    details: { stage: "compensate", rolled_back: false, blocked: true },
  });
  assert.equal(blocked.blocked, true);
  assert.match(blocked.message, /强制 Block/);

  assert.equal(applyCutoverError(pending, { apiCode: "selection_conflict", message: "stale" }).phase, "conflict");
  assert.equal(applyCutoverError(pending, { apiCode: "requires_config_apply", message: "apply" }).phase, "requires_apply");
});

test("Binding source keeps reserved targets and Generation", () => {
  assert.equal(bindingSelection(sourceBinding), "sn_old");
  assert.deepEqual(cutoverSource(sourceBinding), { smart_node: "sn_old", generation: 7 });
  assert.equal(bindingSelection({ ...sourceBinding, target: { kind: "direct" } }), "Direct");
  assert.equal(bindingSelection({ ...sourceBinding, target: { kind: "block" } }), "Block");
});

test("a later Binding Generation supersedes stale success UI", () => {
  const complete = applyCutoverResult(beginCutover("strict", "sn_new"), result());
  const superseded = applyRuntimeProgress(
    complete,
    runtime({
      bindings: [{ ...targetBinding, generation: 9, target: { kind: "block" } }],
    }),
    "general",
  );
  assert.equal(superseded.phase, "superseded");
  assert.equal(superseded.generation, 9);
  assert.match(superseded.message, /Generation 9/);
});
