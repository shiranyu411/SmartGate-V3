import { useEffect, useMemo, useState } from "react";
import {
  useCutover,
  useCores,
  useFailoverEnable,
  useFailoverEvents,
  useFailoverLock,
  useFailoverRevert,
  useFailoverRun,
  useFailoverState,
  useFailoverUnlock,
  useNodes,
  useProxyGroups,
  useRuntimeStatus,
} from "../lib/queries";
import { ApiError } from "../lib/api";
import { errorText } from "../lib/errorText";
import {
  cutoverResultLabel,
  cutoverStageLabel,
  drainErrorLabel,
  failoverKindLabel,
  failoverReasonLabel,
  failoverSourceLabel,
  failoverStatusLabel,
  healthStatusLabel,
} from "../lib/statusText";
import { coreLabel, coreLabelMap, type CoreLabelMap } from "../lib/coreLabels";
import { openEvents } from "../lib/sse";
import { Badge, Dot } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Card, Eyebrow, Rule, SectionHeader } from "../ui/Card";
import { Field, Input } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { ErrorText } from "../ui/ErrorText";
import { eyebrowStyle, sans, serif } from "../ui/tokens";
import type {
  FailoverEvent,
  FailoverPoolState,
  ProxyGroup,
  ProxyGroupCandidate,
  RuntimeStatus,
  SmartNode,
} from "../lib/types";
import {
  applyCutoverError,
  applyCutoverResult,
  applyRuntimeProgress,
  beginCutover,
  bindingSelection,
  cutoverSource,
  isCutoverBusy,
  strictProgressIndex,
} from "./cutoverState";
import type { CutoverViewState } from "./cutoverState";

export function ProxyGroupsPage() {
  const groups = useProxyGroups();
  const runtime = useRuntimeStatus();
  const failoverState = useFailoverState();
  const failoverEvents = useFailoverEvents();
  const nodes = useNodes();
  const cores = useCores();
  const cutover = useCutover();
  const enable = useFailoverEnable();
  const lock = useFailoverLock();
  const unlock = useFailoverUnlock();
  const revert = useFailoverRevert();
  const run = useFailoverRun();

  const [error, setError] = useState<unknown>(null);
  const [lockTarget, setLockTarget] = useState<string | null>(null);
  const [liveRuntime, setLiveRuntime] = useState<RuntimeStatus | null>(null);
  const [sseState, setSseState] = useState<"connecting" | "live" | "down">("connecting");
  const [cutovers, setCutovers] = useState<Record<string, CutoverViewState>>({});
  const [nowMs, setNowMs] = useState(() => Date.now());

  useEffect(() => {
    const close = openEvents({
      onOpen: () => setSseState("live"),
      onStatus: (event) => {
        const nextRuntime = event.runtime;
        if (!isRuntimeStatus(nextRuntime)) return;
        setLiveRuntime(nextRuntime);
        setCutovers((current) => reconcileCutovers(current, nextRuntime));
        setSseState("live");
      },
      onError: () => {
        setLiveRuntime(null);
        setSseState("down");
      },
    });
    return close;
  }, []);

  useEffect(() => {
    if (!runtime.data) return;
    setCutovers((current) => reconcileCutovers(current, runtime.data));
  }, [runtime.data]);

  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);

  const list = groups.data?.groups ?? [];
  const activeRuntime = liveRuntime ?? runtime.data;
  const nodeMap = useMemo(() => {
    const map = new Map<string, SmartNode>();
    for (const node of nodes.data?.nodes ?? []) map.set(node.id, node);
    return map;
  }, [nodes.data]);
  const coreLabels = useMemo(() => coreLabelMap(cores.data), [cores.data]);
  const events = useMemo(() => failoverEvents.data?.events ?? [], [failoverEvents.data?.events]);
  const latestEvents = useMemo(() => {
    const map = new Map<string, FailoverEvent>();
    for (let index = events.length - 1; index >= 0; index -= 1) {
      const event = events[index];
      if (!map.has(event.pool_id)) map.set(event.pool_id, event);
    }
    return map;
  }, [events]);

  async function call<T>(promise: Promise<T>) {
    setError(null);
    try {
      await promise;
    } catch (caught) {
      setError(caught);
    }
  }

  async function selectTarget(group: ProxyGroup, target: string) {
    if (!activeRuntime || activeRuntime.config_epoch <= 0) {
      setError("运行状态尚未加载，拒绝构造不完整的 Cutover CAS 请求");
      return;
    }
    setError(null);
    setCutovers((current) => ({ ...current, [group.pool]: beginCutover("unknown", target) }));
    try {
      const result = await cutover.mutateAsync({
        poolId: group.pool,
        targetSmartNode: target,
        source: cutoverSource(group.binding),
        expectedConfigEpoch: activeRuntime.config_epoch,
      });
      setCutovers((current) => ({
        ...current,
        [group.pool]: applyCutoverResult(current[group.pool] ?? beginCutover("unknown", target), result),
      }));
    } catch (caught) {
      const failure = caught instanceof ApiError
        ? { apiCode: caught.apiCode, message: caught.message, details: caught.details }
        : { message: errorText(caught) };
      setCutovers((current) => ({
        ...current,
        [group.pool]: applyCutoverError(current[group.pool] ?? beginCutover("unknown", target), failure),
      }));
    }
  }

  return (
    <div className="space-y-12 max-w-[1120px]">
      <SectionHeader
        eyebrow="代理 · Proxy Groups"
        title="Binding 与故障切换"
        desc="每次选择都以 SmartNode + Generation 做 CAS，并通过 Pool 级 CutoverTxn 收敛真实出口。"
        right={
          <div className="flex items-center gap-2 flex-wrap justify-end">
            <Badge tone={sseState === "live" ? "ok" : sseState === "down" ? "warn" : "muted"}>
              <Dot tone={sseState === "live" ? "ok" : sseState === "down" ? "warn" : "muted"} />
              排空进度 {sseState === "live" ? "实时" : sseState === "down" ? "轮询回退" : "连接中"}
            </Badge>
            <Badge tone={failoverState.data?.enabled ? "ok" : "muted"}>
              <Dot tone={failoverState.data?.enabled ? "ok" : "muted"} />
              自动切换 {failoverState.data?.enabled ? "已开启" : "已关闭"}
            </Badge>
            <Button size="sm" variant="ghost" onClick={() => call(run.mutateAsync())} loading={run.isPending}>
              立即评估
            </Button>
            <Button
              size="sm"
              onClick={() => call(enable.mutateAsync(!failoverState.data?.enabled))}
              loading={enable.isPending}
            >
              {failoverState.data?.enabled ? "关闭自动切换" : "开启自动切换"}
            </Button>
          </div>
        }
      />

      {error ? <ErrorBanner error={error} /> : null}
      {groups.error ? <ErrorBanner error={groups.error} /> : null}

      <Card>
        <ul>
          {groups.isLoading ? <Placeholder>载入 Binding…</Placeholder> : null}
          {!groups.isLoading && list.length === 0 ? <Placeholder>暂无 Applied Pool Binding。</Placeholder> : null}
          {list.map((group, index) => {
            const poolID = group.pool;
            const state = cutovers[poolID];
            const mutationBusy = cutover.isPending && cutover.variables?.poolId === poolID;
            return (
              <GroupRow
                key={poolID}
                group={group}
                first={index === 0}
                last={index === list.length - 1}
                poolState={failoverState.data?.pools?.[poolID]}
                latestEvent={latestEvents.get(poolID)}
                nodeMap={nodeMap}
                coreLabels={coreLabels}
                cutoverState={state}
                nowMs={nowMs}
                onSelect={(target) => void selectTarget(group, target)}
                onLock={() => setLockTarget(poolID)}
                onUnlock={() => call(unlock.mutateAsync(poolID))}
                onRevert={() => {
                  setCutovers((current) => {
                    const next = { ...current };
                    delete next[poolID];
                    return next;
                  });
                  void call(revert.mutateAsync(poolID));
                }}
                busy={mutationBusy || isCutoverBusy(state) || lock.isPending || unlock.isPending || revert.isPending}
                cutoverReady={Boolean(activeRuntime && activeRuntime.config_epoch > 0)}
              />
            );
          })}
        </ul>
      </Card>

      <Rule />

      <section className="space-y-5">
        <SectionHeader
          eyebrow="故障切换 · 事件"
          title="最近事件"
          desc="自动切换、人工 revert 与 Worker 故障均记录前后 Generation、Executor 和 Cutover 结果。"
        />
        <Card>
          {failoverEvents.isLoading ? (
            <Placeholder>载入事件…</Placeholder>
          ) : events.length === 0 ? (
            <Placeholder>尚无故障切换事件。</Placeholder>
          ) : (
            <ul>
              {events
                .slice()
                .reverse()
                .map((event, index, reversed) => (
                  <EventRow key={`${event.time}-${index}`} event={event} last={index === reversed.length - 1} />
                ))}
            </ul>
          )}
        </Card>
      </section>

      <Footer />

      <LockModal
        open={lockTarget !== null}
        onClose={() => setLockTarget(null)}
        onSubmit={async (reason) => {
          if (!lockTarget) return;
          await call(lock.mutateAsync({ poolId: lockTarget, reason }));
          setLockTarget(null);
        }}
        loading={lock.isPending}
      />
    </div>
  );
}

function GroupRow({
  group,
  first,
  last,
  poolState,
  latestEvent,
  nodeMap,
  coreLabels,
  cutoverState,
  nowMs,
  onSelect,
  onLock,
  onUnlock,
  onRevert,
  busy,
  cutoverReady,
}: {
  group: ProxyGroup;
  first: boolean;
  last: boolean;
  poolState?: FailoverPoolState;
  latestEvent?: FailoverEvent;
  nodeMap: Map<string, SmartNode>;
  coreLabels: CoreLabelMap;
  cutoverState?: CutoverViewState;
  nowMs: number;
  onSelect: (target: string) => void;
  onLock: () => void;
  onUnlock: () => void;
  onRevert: () => void;
  busy: boolean;
  cutoverReady: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [confirmControl, setConfirmControl] = useState<string | null>(null);
  const selected = bindingSelection(group.binding);
  const currentlyBlocked = selected === "Block";
  const cooldown = cooldownText(group.cooldown_until, nowMs);
  const locked = group.lock !== null;
  return (
    <li
      data-pool={group.pool}
      style={{
        borderTop: first ? "none" : "1px solid var(--border)",
        borderBottom: last ? "none" : undefined,
      }}
    >
      <div className="px-7 py-5 flex items-start justify-between gap-5 flex-wrap">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2.5 flex-wrap">
            <p className="text-[18px]" style={{ ...serif, color: "var(--fg-strong)" }}>
              {group.pool}
            </p>
            <Badge tone="muted">Generation {group.binding.generation}</Badge>
            {group.binding.target.core ? <Badge tone="info">{coreLabel(group.binding.target.core, coreLabels)}</Badge> : null}
            {currentlyBlocked ? <Badge tone="err">已断开 · fail-closed</Badge> : null}
            {locked ? <Badge tone="err">已锁定 {group.lock?.reason ? `· ${group.lock.reason}` : ""}</Badge> : null}
            {cooldown ? <Badge tone="warn">{cooldown}</Badge> : null}
            <Badge tone={group.failover_enabled ? "ok" : "muted"}>
              自动切换 {group.failover_enabled ? "已开启" : "已关闭"}
            </Badge>
          </div>
          <p className="mt-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
            当前 <strong style={{ color: "var(--fg-strong)" }}>{labelNode(selected, nodeMap)}</strong>
            {group.binding.target.worker ? ` · ${group.binding.target.worker}` : ""}
            {group.binding.target.executor ? ` · ${group.binding.target.executor}` : ""}
          </p>
          <p className="mt-1 text-[11px]" style={{ ...sans, color: "var(--muted)" }}>
            Binding <code style={{ fontFamily: "var(--font-mono)" }}>{group.binding.id}</code> · 自 {fmtTime(group.binding.since)} · 候选 {group.candidates.length} 个
          </p>
          {latestEvent ? <EventSummary event={latestEvent} /> : poolState ? <PoolStateSummary state={poolState} /> : null}
        </div>
        <div className="flex flex-wrap items-center gap-2 shrink-0">
          <Button size="sm" variant="ghost" onClick={() => setOpen((value) => !value)}>
            {open ? "收起" : "切换节点"}
          </Button>
          {locked ? (
            <Button size="sm" variant="ghost" onClick={onUnlock} loading={busy}>
              解除锁定
            </Button>
          ) : (
            <Button size="sm" variant="ghost" onClick={onLock} loading={busy}>
              锁定
            </Button>
          )}
          {poolState?.previous ? (
            <Button size="sm" variant="ghost" onClick={onRevert} loading={busy} title={`回滚到 ${poolState.previous}`}>
              回滚
            </Button>
          ) : null}
        </div>
      </div>

      {cutoverState ? <CutoverStatus state={cutoverState} /> : null}

      {open ? (
        <div className="px-7 py-5" style={{ background: "var(--surface-2)", borderTop: "1px solid var(--border)" }}>
          <p style={eyebrowStyle}>Applied 候选节点</p>
          <div className="mt-3 flex flex-wrap gap-2">
            {group.candidates.length === 0 ? (
              <span className="text-xs" style={{ ...sans, color: "var(--muted)" }}>
                无可切换候选
              </span>
            ) : (
              group.candidates.map((candidate) => {
                const target = candidate.smart_node;
                const active = target === selected;
                const control = controlMeta(target);
                const node = nodeMap.get(target);
                const controlColor = target === "Block" ? "var(--danger)" : "var(--warning)";
                return (
                  <button
                    key={target}
                    data-cutover-target={target}
                    type="button"
                    onClick={() => {
                      if (active) return;
                      if (control) setConfirmControl(target);
                      else onSelect(target);
                    }}
                    disabled={active || busy || !cutoverReady}
                    title={control?.hint ?? node?.display_name ?? target}
                    className="px-3 py-2 text-xs transition-colors disabled:cursor-default rounded-[6px]"
                    style={{
                      background: active ? "var(--accent)" : "var(--surface)",
                      color: active ? "var(--accent-fg)" : control ? controlColor : "var(--fg-strong)",
                      border: `1px solid ${
                        active
                          ? "var(--accent)"
                          : control
                            ? `color-mix(in srgb, ${controlColor} 45%, transparent)`
                            : "var(--border)"
                      }`,
                      ...sans,
                    }}
                  >
                    <span className="inline-flex items-center gap-2 flex-wrap justify-center">
                      <span>{control ? control.label : labelNode(target, nodeMap)}</span>
                      {candidate.core ? <Badge tone="info">{coreLabel(candidate.core, coreLabels)}</Badge> : null}
                      <Badge tone={healthTone(candidate.health)}>{healthLabel(candidate)}</Badge>
                    </span>
                  </button>
                );
              })
            )}
          </div>
          {!cutoverReady ? (
            <p className="mt-3 text-xs" style={{ ...sans, color: "var(--warning)" }}>
              Runtime config_epoch 尚未加载，切换按钮暂时禁用，避免发送不完整 CAS。
            </p>
          ) : null}
          {confirmControl ? (
            <ConfirmControl
              target={confirmControl}
              onCancel={() => setConfirmControl(null)}
              onConfirm={() => {
                onSelect(confirmControl);
                setConfirmControl(null);
              }}
            />
          ) : null}
        </div>
      ) : null}
    </li>
  );
}

function CutoverStatus({ state }: { state: CutoverViewState }) {
  const tone = state.phase === "complete"
    ? "ok"
    : state.phase === "partial" || state.phase === "requires_apply"
      ? "warn"
      : state.phase === "failed" || state.phase === "drain_failed"
        ? "err"
        : state.phase === "conflict" || state.phase === "superseded"
          ? "info"
          : "accent";
  const active = isCutoverBusy(state);
  const progressIndex = strictProgressIndex(state);
  return (
    <div
      className="mx-7 mb-5 px-4 py-3"
      style={{
        background: `color-mix(in srgb, ${toneColor(tone)} 8%, transparent)`,
        border: `1px solid color-mix(in srgb, ${toneColor(tone)} 32%, transparent)`,
        borderRadius: "var(--radius-sm)",
      }}
    >
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <p className={active ? "pulse-soft text-sm" : "text-sm"} style={{ ...sans, color: toneColor(tone) }}>
          {state.message}
        </p>
        <div className="flex items-center gap-2 flex-wrap">
          {state.generation ? <Badge tone="muted">Generation {state.generation}</Badge> : null}
          {state.stage ? <span title={state.stage}><Badge tone={tone}>{cutoverStageLabel(state.stage, state.phase)}</Badge></span> : null}
          {state.rolledBack ? <Badge tone="ok">已回滚</Badge> : null}
          {state.blocked ? <Badge tone="err">Pool 已阻断</Badge> : null}
        </div>
      </div>
      {state.policy === "strict" && progressIndex >= 0 ? (
        <div className="mt-3 flex items-center gap-2 flex-wrap">
          {["准备执行路径", "关闭旧连接", "出口验证"].map((label, index) => (
            <Badge key={label} tone={progressIndex > index ? "ok" : progressIndex === index ? "accent" : "muted"}>
              {index + 1}. {label}
            </Badge>
          ))}
        </div>
      ) : null}
      {["draining", "retrying", "drain_failed"].includes(state.phase) ? (
        <p className="mt-2 text-xs" style={{ ...sans, color: "var(--muted)" }}>
          旧 Generation {state.oldGeneration ?? "—"} · 剩余 {state.remaining ?? "—"} 条
          {state.deadline ? ` · 最迟 ${fmtTime(state.deadline)}` : ""}
          {state.retryCount ? ` · 已重试 ${state.retryCount} 次` : ""}
        </p>
      ) : null}
      {state.lastError ? (
        <p className="mt-2 break-words text-xs" style={{ ...sans, color: "var(--danger)" }}>
          最后错误：{drainErrorLabel(state.lastError)}
          {drainErrorLabel(state.lastError) !== state.lastError ? (
            <span className="mt-1 block" style={{ color: "var(--muted)" }}>
              诊断：{state.lastError}
            </span>
          ) : null}
        </p>
      ) : null}
      {state.closedConnections !== undefined ? (
        <p className="mt-2 text-xs" style={{ ...sans, color: "var(--muted)" }}>
          精确关闭旧 Generation 连接 {state.closedConnections} 条
        </p>
      ) : null}
    </div>
  );
}

function EventSummary({ event }: { event: FailoverEvent }) {
  return (
    <p className="mt-2 text-[11px] italic" style={{ ...serif, color: "var(--muted)" }}>
      最近事件 {fmtTime(event.time)} ·{" "}
      <span title={event.kind || event.status}>
        {event.kind ? failoverKindLabel(event.kind) : failoverStatusLabel(event.status)}
      </span>{" "}
      · <span title={event.reason}>{failoverReasonLabel(event.reason)}</span>
      {event.generation_from || event.generation_to ? ` · Gen ${event.generation_from ?? "—"}→${event.generation_to ?? "—"}` : ""}
      {event.cutover_result ? <span title={event.cutover_result}> · {cutoverResultLabel(event.cutover_result)}</span> : null}
    </p>
  );
}

function PoolStateSummary({ state }: { state: FailoverPoolState }) {
  return (
    <p className="mt-2 text-[11px] italic" style={{ ...serif, color: "var(--muted)" }}>
      上次变更 {fmtTime(state.last_changed_at)} ·{" "}
      <span title={state.last_kind}>{failoverKindLabel(state.last_kind)}</span> ·{" "}
      <span title={state.last_reason}>{failoverReasonLabel(state.last_reason)}</span>
    </p>
  );
}

function ConfirmControl({ target, onCancel, onConfirm }: { target: string; onCancel: () => void; onConfirm: () => void }) {
  const meta = controlMeta(target);
  const color = target === "Block" ? "var(--danger)" : "var(--warning)";
  return (
    <div
      className="mt-3 px-3 py-2 flex items-center justify-between gap-3 flex-wrap"
      style={{
        background: `color-mix(in srgb, ${color} 8%, transparent)`,
        border: `1px solid color-mix(in srgb, ${color} 35%, transparent)`,
      }}
    >
      <p className="text-xs min-w-0" style={{ ...sans, color: "var(--fg)" }}>
        {meta?.hint}。确认把 Binding 切到 {target === "Direct" ? "直连 (Direct)" : target === "Block" ? "阻断 (Block)" : target}？
      </p>
      <div className="flex gap-2 shrink-0">
        <Button size="sm" variant="ghost" onClick={onCancel}>
          取消
        </Button>
        <Button size="sm" variant="danger" onClick={onConfirm}>
          确认切换
        </Button>
      </div>
    </div>
  );
}

function EventRow({ event, last }: { event: FailoverEvent; last: boolean }) {
  const tone = event.kind === "fail_closed" ? "warn" : event.reason === "manual_revert" ? "info" : event.status === "failed" ? "err" : "ok";
  return (
    <li className="px-7 py-4 flex items-baseline gap-4" style={{ borderBottom: last ? "none" : "1px solid var(--border)" }}>
      <span className="text-[11px] tabular-nums shrink-0 w-32" style={{ ...sans, color: "var(--muted)" }}>
        {fmtTime(event.time)}
      </span>
      <div className="flex-1 min-w-0">
        <p className="text-[14px]" style={{ ...serif, color: "var(--fg-strong)" }}>
          {event.pool_id} <span style={{ color: "var(--muted)" }}>·</span> {failoverTargetLabel(event.from)} → {failoverTargetLabel(event.to)}
        </p>
        <p className="text-xs mt-0.5 break-words" style={{ ...sans, color: "var(--muted)" }}>
          <span title={event.reason}>{failoverReasonLabel(event.reason)}</span>
          {event.generation_from || event.generation_to ? ` · Gen ${event.generation_from ?? "—"}→${event.generation_to ?? "—"}` : ""}
          {event.executor ? ` · ${event.executor}` : ""}
          {event.cutover_result ? <span title={event.cutover_result}> · {cutoverResultLabel(event.cutover_result)}</span> : null}
          {event.source ? <span title={event.source}> · {failoverSourceLabel(event.source)}</span> : null}
          {event.error ? ` · 诊断：${event.error}` : ""}
        </p>
      </div>
      <span title={event.kind || event.status}>
        <Badge tone={tone}>{event.kind ? failoverKindLabel(event.kind) : failoverStatusLabel(event.status)}</Badge>
      </span>
    </li>
  );
}

function LockModal({
  open,
  onClose,
  onSubmit,
  loading,
}: {
  open: boolean;
  onClose: () => void;
  onSubmit: (reason: string) => void;
  loading: boolean;
}) {
  const [reason, setReason] = useState("维护");
  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="锁定业务池"
      title="暂停自动切换"
      width={520}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            取消
          </Button>
          <Button onClick={() => onSubmit(reason)} loading={loading}>
            锁定
          </Button>
        </>
      }
    >
      <p className="text-sm mb-4" style={{ ...sans, color: "var(--fg)" }}>
        锁定后，自动故障切换会跳过该业务池。请输入原因，便于后续审计。
      </p>
      <Field label="原因">
        <Input value={reason} onChange={(event) => setReason(event.target.value)} placeholder="例如：维护、调试" />
      </Field>
    </Modal>
  );
}

function ErrorBanner({ error }: { error: unknown }) {
  return (
    <div
      className="px-5 py-3"
      style={{
        background: "color-mix(in srgb, var(--danger) 8%, transparent)",
        border: "1px solid color-mix(in srgb, var(--danger) 35%, transparent)",
      }}
    >
      <p className="break-words text-sm" style={{ ...sans, color: "var(--danger)" }}>
        <ErrorText error={error} fallback="选择器操作失败" showDiagnostic />
      </p>
    </div>
  );
}

function Placeholder({ children }: { children: React.ReactNode }) {
  return (
    <div className="px-7 py-10 text-center">
      <p className="text-sm" style={{ ...sans, color: "var(--muted)" }}>
        {children}
      </p>
    </div>
  );
}

function reconcileCutovers(current: Record<string, CutoverViewState>, runtime: RuntimeStatus): Record<string, CutoverViewState> {
  let changed = false;
  const next = { ...current };
  for (const [pool, state] of Object.entries(current)) {
    const updated = applyRuntimeProgress(state, runtime, pool);
    if (updated !== state) {
      next[pool] = updated;
      changed = true;
    }
  }
  return changed ? next : current;
}

function isRuntimeStatus(value: Partial<RuntimeStatus> | undefined): value is RuntimeStatus {
  return Boolean(
    value &&
      typeof value.config_epoch === "number" &&
      Array.isArray(value.bindings) &&
      Array.isArray(value.cutovers_in_flight) &&
      Array.isArray(value.drains),
  );
}

function labelNode(id: string, map: Map<string, SmartNode>): string {
  if (!id) return "—";
  if (id === "Direct") return "直连 (Direct)";
  if (id === "Block") return "阻断 (Block)";
  const node = map.get(id);
  return node?.display_name || id;
}

/* 故障切换事件里的 from/to：保留节点原始 ID，仅翻译保留字 */
function failoverTargetLabel(target?: string): string {
  if (!target) return "—";
  if (target === "Direct") return "直连";
  if (target === "Block") return "阻断";
  return target;
}

function controlMeta(target: string): { label: string; hint: string } | null {
  if (target === "Block") {
    return { label: "Block · 阻断全部", hint: "选择后该 Pool 的全部流量都会被丢弃（fail-closed 保护）" };
  }
  if (target === "Direct") {
    return { label: "Direct · 直连", hint: "绕过代理直连，可能暴露真实 IP 或位置" };
  }
  return null;
}

function healthLabel(candidate: ProxyGroupCandidate): string {
  const status = healthStatusLabel(candidate.health);
  if (candidate.delay_ms !== undefined && candidate.delay_ms > 0) return `${status} · ${candidate.delay_ms} ms`;
  return status;
}

function healthTone(health: string): "ok" | "warn" | "err" | "muted" {
  if (health === "ok") return "ok";
  if (health === "failed") return "err";
  if (health === "degraded") return "warn";
  return "muted";
}

function cooldownText(value: string | null, nowMs: number): string {
  if (!value) return "";
  const deadline = new Date(value).getTime();
  if (!Number.isFinite(deadline)) return "";
  const seconds = Math.ceil((deadline - nowMs) / 1_000);
  if (seconds <= 0) return "";
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return `冷却 ${minutes}:${String(remainder).padStart(2, "0")}`;
}

function toneColor(tone: "ok" | "warn" | "err" | "info" | "muted" | "accent"): string {
  if (tone === "ok") return "var(--success)";
  if (tone === "warn") return "var(--warning)";
  if (tone === "err") return "var(--danger)";
  if (tone === "info") return "var(--info)";
  if (tone === "accent") return "var(--accent)";
  return "var(--muted)";
}

function fmtTime(value: string | undefined | null): string {
  if (!value || value === "unknown") return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString();
}

function Footer() {
  return (
    <Eyebrow>
      <span style={{ letterSpacing: "0.3em" }}>SmartGate · 书房 · Proxy Groups & Failover</span>
    </Eyebrow>
  );
}
