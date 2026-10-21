import type {
  CutoverFailureDetails,
  CutoverPolicy,
  CutoverResult,
  CutoverSource,
  PoolBinding,
  RuntimeStatus,
} from "../lib/types.js";

export type CutoverPhase =
  | "preparing"
  | "closing"
  | "verifying"
  | "draining"
  | "retrying"
  | "drain_failed"
  | "complete"
  | "partial"
  | "failed"
  | "conflict"
  | "requires_apply"
  | "superseded";

export type CutoverViewState = {
  phase: CutoverPhase;
  policy: CutoverPolicy | "unknown";
  target: string;
  message: string;
  stage?: string;
  generation?: number;
  closedConnections?: number;
  residualConnections?: number;
  oldGeneration?: number;
  remaining?: number;
  deadline?: string;
  retryCount?: number;
  lastError?: string;
  drainObserved?: boolean;
  rolledBack?: boolean;
  blocked?: boolean;
};

export type CutoverErrorLike = {
  apiCode?: string;
  message: string;
  details?: unknown;
};

export function beginCutover(policy: CutoverPolicy | "unknown", target: string): CutoverViewState {
  return {
    phase: "preparing",
    policy,
    target,
    message: "准备执行路径",
    stage: "requested",
  };
}

export function applyRuntimeProgress(
  current: CutoverViewState,
  runtime: Pick<RuntimeStatus, "bindings" | "cutovers_in_flight" | "drains">,
  pool: string,
): CutoverViewState {
  const liveBinding = runtime.bindings.find((item) => item.pool === pool);
  if (current.generation !== undefined && liveBinding && liveBinding.generation > current.generation) {
    return {
      ...current,
      phase: "superseded",
      message: `该切换结果已被后续 Binding Generation ${liveBinding.generation} 取代`,
      stage: "binding_superseded",
      generation: liveBinding.generation,
    };
  }
  if (isTerminal(current.phase)) return current;

  const drain = runtime.drains.find((item) => item.pool === pool);
  if (drain) {
    const phase = drain.status === "failed" ? "drain_failed" : drain.status;
    if (
      current.phase === phase &&
      current.remaining === drain.remaining &&
      current.deadline === drain.deadline &&
      current.oldGeneration === drain.old_generation &&
      current.retryCount === drain.retry_count &&
      current.lastError === drain.last_error
    ) {
      return current;
    }
    const message = drain.status === "failed"
      ? "旧 Generation 清理失败，状态将保留并阻止后续切换"
      : drain.status === "retrying"
        ? `旧 Generation 清理失败，正在重试（第 ${drain.retry_count} 次）`
        : `已切换，旧连接排空中（剩余 ${drain.remaining}）`;
    return {
      ...current,
      phase,
      policy: "graceful",
      message,
      stage: drain.status,
      oldGeneration: drain.old_generation,
      remaining: drain.remaining,
      deadline: drain.deadline,
      retryCount: drain.retry_count,
      lastError: drain.last_error,
      drainObserved: true,
    };
  }

  if (current.phase === "drain_failed" && current.drainObserved) {
    return {
      ...current,
      phase: "complete",
      message: "旧 Generation 清理已恢复完成",
      stage: "drain_recovered",
      remaining: 0,
      lastError: undefined,
    };
  }

  const progress = runtime.cutovers_in_flight.find((item) => item.pool === pool);
  if (progress) {
    const next = stateForStage(current, progress.stage);
    if (next.phase === current.phase && next.stage === current.stage && next.message === current.message) {
      return current;
    }
    return next;
  }

  if ((current.phase === "draining" || current.phase === "retrying") && current.drainObserved) {
    return {
      ...current,
      phase: "complete",
      message: "旧 Generation 连接已排空",
      stage: "drain_complete",
      remaining: 0,
    };
  }

  return current;
}

export function applyCutoverResult(current: CutoverViewState, result: CutoverResult): CutoverViewState {
  if (current.phase === "complete") return current;

  if (result.result === "cutover_partial") {
    const residual = result.residual_connections ?? 0;
    return {
      ...current,
      phase: "partial",
      message: `已切换但存在 ${residual} 条残留连接`,
      stage: "residual_connections",
      generation: result.binding.generation,
      residualConnections: residual,
    };
  }

  if (result.result === "cutover_complete" && result.verified === true) {
    return {
      ...current,
      phase: "complete",
      policy: current.policy === "unknown" ? "strict" : current.policy,
      message: "旧连接已清理，新出口验证通过",
      stage: "cutover_complete",
      generation: result.binding.generation,
      closedConnections: result.closed_connections ?? 0,
    };
  }

  if (result.result !== "committed" || result.verified !== true || !result.draining) {
    return {
      ...current,
      phase: "failed",
      message: "切换响应未满足完成条件，未标记为成功",
      stage: "invalid_cutover_result",
    };
  }

  if (result.draining.remaining === 0) {
    return {
      ...current,
      phase: "complete",
      policy: "graceful",
      message: "Binding 已切换，旧连接无需排空",
      stage: "drain_complete",
      generation: result.binding.generation,
      remaining: 0,
    };
  }

  return {
    ...current,
    phase: "draining",
    policy: "graceful",
    message: `已切换，旧连接排空中（剩余 ${result.draining.remaining}）`,
    stage: "draining",
    generation: result.binding.generation,
    oldGeneration: result.draining.old_generation,
    remaining: result.draining.remaining,
    deadline: result.draining.deadline,
    drainObserved: true,
  };
}

export function applyCutoverError(current: CutoverViewState, error: CutoverErrorLike): CutoverViewState {
  const details = asDetails(error.details);
  const stage = stringField(details, "stage");
  if (error.apiCode === "selection_conflict") {
    return {
      ...current,
      phase: "conflict",
      message: "实时 Binding 已变化，已刷新状态，请重新选择",
      stage: stage || "selection_conflict",
    };
  }
  if (error.apiCode === "requires_config_apply") {
    return {
      ...current,
      phase: "requires_apply",
      message: "目标执行路径尚未进入 Applied graph，请先完成配置 Apply",
      stage: stage || "requires_config_apply",
    };
  }
  if (error.apiCode === "cutover_conflict") {
    return {
      ...current,
      phase: "conflict",
      message: "该 Pool 正在切换或排空，请等待当前操作结束",
      stage: stage || "cutover_conflict",
    };
  }
  if (error.apiCode === "cutover_failed") {
    const rolledBack = booleanField(details, "rolled_back");
    const blocked = booleanField(details, "blocked");
    const message = blocked
      ? "切换失败且补偿未完成，Pool 已强制 Block"
      : rolledBack
        ? "切换失败，已回滚到原 Binding"
        : "切换失败，未确认回滚完成，请检查事务恢复状态";
    return {
      ...current,
      phase: "failed",
      message,
      stage: stage || "cutover_failed",
      rolledBack,
      blocked,
    };
  }
  return {
    ...current,
    phase: "failed",
    // 后端返回中文消息则直接展示；英文原文不给用户当主文案
    message: error.message && /[㐀-鿿]/u.test(error.message) ? error.message : "切换失败",
    stage: stage || "request_failed",
  };
}

export function bindingSelection(binding: PoolBinding): string {
  if (binding.target.kind === "direct") return "Direct";
  if (binding.target.kind === "block") return "Block";
  return binding.target.smart_node || "";
}

export function cutoverSource(binding: PoolBinding): CutoverSource {
  return { smart_node: bindingSelection(binding), generation: binding.generation };
}

export function isCutoverBusy(state: CutoverViewState | undefined): boolean {
  if (!state) return false;
  return ["preparing", "closing", "verifying", "draining", "retrying", "drain_failed"].includes(state.phase);
}

export function strictProgressIndex(state: CutoverViewState): number {
  if (state.phase === "preparing") return 0;
  if (state.phase === "closing") return 1;
  if (state.phase === "verifying") return 2;
  if (state.phase === "complete" || state.phase === "partial") return 3;
  return -1;
}

function stateForStage(current: CutoverViewState, stage: string): CutoverViewState {
  if (["close_old_binding", "resolve_source", "fabric_switch"].includes(stage)) {
    return { ...current, phase: "closing", policy: "strict", message: "关闭旧 Generation 连接", stage };
  }
  if (["verify_new_path", "commit_target"].includes(stage)) {
    return { ...current, phase: "verifying", policy: "strict", message: "验证新 Binding 出口", stage };
  }
  if (stage === "start_drain" || stage === "draining") {
    return { ...current, phase: "draining", policy: "graceful", message: "Binding 已提交，等待排空进度", stage };
  }
  return { ...current, phase: "preparing", message: "准备执行路径", stage };
}

function isTerminal(phase: CutoverPhase): boolean {
  return ["complete", "partial", "failed", "conflict", "requires_apply", "superseded"].includes(phase);
}

function asDetails(value: unknown): CutoverFailureDetails & Record<string, unknown> {
  if (!value || typeof value !== "object") return {};
  return value as CutoverFailureDetails & Record<string, unknown>;
}

function stringField(value: Record<string, unknown>, key: string): string | undefined {
  return typeof value[key] === "string" ? value[key] : undefined;
}

function booleanField(value: Record<string, unknown>, key: string): boolean {
  return value[key] === true;
}
