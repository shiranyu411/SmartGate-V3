import { useEffect, useState } from "react";
import { Modal } from "../ui/Modal";
import { Button } from "../ui/Button";
import { Badge } from "../ui/Badge";
import { ApiError, makeOpId } from "../lib/api";
import { errorText } from "../lib/errorText";
import { applyStatusLabel, dryRunCategoryLabel, resultStatusLabel, rollbackStageLabel } from "../lib/statusText";
import { useCores, useDryRun, useApply, useDiscard } from "../lib/queries";
import { coreLabel, coreLabelMap } from "../lib/coreLabels";
import { gatewayModeLabel } from "../pages/gatewayState";
import type { DryRunResult, ApplyResult, ApplyRollbackReport } from "../lib/types";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

type Step = "intro" | "checking" | "preview" | "applying" | "done" | "error";

export function DryRunModal({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const dryrun = useDryRun();
  const apply = useApply();
  const discard = useDiscard();

  const [step, setStep] = useState<Step>("intro");
  const [plan, setPlan] = useState<DryRunResult | null>(null);
  const [result, setResult] = useState<ApplyResult | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [rollback, setRollback] = useState<ApplyRollbackReport | null>(null);

  useEffect(() => {
    if (!open) {
      setStep("intro");
      setPlan(null);
      setResult(null);
      setError(null);
      setRollback(null);
    }
  }, [open]);

  async function runDryrun() {
    setError(null);
    setPlan(null);
    setRollback(null);
    setStep("checking");
    try {
      const data = await dryrun.mutateAsync({ opId: makeOpId("config") });
      setPlan(data);
      setStep("preview");
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.details && typeof e.details === "object") {
        const details = e.details as { dryrun?: DryRunResult };
        if (details.dryrun && Array.isArray(details.dryrun.changes)) {
          setPlan(details.dryrun);
        }
      }
      setStep("error");
    }
  }

  async function runApply() {
    if (!plan || plan.changes_complete !== true || !plan.changes || plan.changes.some((change) => change.blocking)) return;
    setStep("applying");
    setError(null);
    setRollback(null);
    try {
      const data = await apply.mutateAsync({
        dryrunId: plan.id,
        description: `gateway_mode=${plan.gateway_mode}`,
      });
      setResult(data);
      setStep("done");
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.details && typeof e.details === "object") {
        const details = e.details as { rollback?: ApplyRollbackReport };
        if (details.rollback) setRollback(details.rollback);
      }
      setStep("error");
    }
  }

  async function runDiscard() {
    try {
      await discard.mutateAsync();
    } catch {
      /* 忽略 */
    }
    onClose();
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="配置应用"
      title={titleOf(step)}
      width={720}
      footer={<Footer
        step={step}
        plan={plan}
        onClose={onClose}
        onStart={runDryrun}
        onApply={runApply}
        onDiscard={runDiscard}
      />}
    >
      <Body step={step} plan={plan} result={result} error={error} rollback={rollback} />
    </Modal>
  );
}

function titleOf(step: Step) {
  switch (step) {
    case "intro":
      return "应用待定的配置";
    case "checking":
      return "正在演练 (dry-run)";
    case "preview":
      return "请确认即将应用的变更";
    case "applying":
      return "正在落地配置";
    case "done":
      return "配置已生效";
    case "error":
      return "出现问题";
  }
}

function Body({
  step,
  plan,
  result,
  error,
  rollback,
}: {
  step: Step;
  plan: DryRunResult | null;
  result: ApplyResult | null;
  error: unknown;
  rollback: ApplyRollbackReport | null;
}) {
  if (step === "intro") {
    return (
      <div className="space-y-4">
        <p className="text-[15px] leading-7" style={{ ...serif, color: "var(--fg)" }}>
          为了避免半生效状态，SmartGate 把"修改"和"应用"分成两步：
        </p>
        <ol className="text-sm space-y-2 pl-5 list-decimal" style={{ ...sans, color: "var(--fg)" }}>
          <li>先做一次 <em style={serif}>dry-run</em>，由后端生成各 Worker 候选配置并执行校验。</li>
          <li>你确认计划后，再发起 <em style={serif}>apply</em>，由后端落地配置、更新 Worker、执行 OpenWrt 防火墙/DNS 命令。</li>
          <li>过程中如发生错误，<em style={serif}>apply</em> 会回滚或保留可恢复点。</li>
        </ol>
      </div>
    );
  }

  if (step === "checking") {
    return <Placeholder text="正在生成并校验各 Worker 候选配置…" />;
  }

  if (step === "applying") {
    return <Placeholder text="正在写入 Worker 配置并应用 OpenWrt 计划…" />;
  }

  if (step === "preview" && plan) {
    return <PlanPreview plan={plan} />;
  }

  if (step === "done" && result) {
    return <DonePanel result={result} />;
  }

  if (step === "error") {
    return (
      <div className="space-y-4">
        <div
          className="px-5 py-4"
          style={{
            background: "color-mix(in srgb, var(--danger) 8%, transparent)",
            border: "1px solid color-mix(in srgb, var(--danger) 35%, transparent)",
            borderRadius: "var(--radius-md)",
          }}
        >
          <p style={eyebrowStyle}>错误</p>
          <p className="break-words mt-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
            <ErrorText error={error} fallback="配置应用失败" showDiagnostic />
          </p>
        </div>
        {plan?.changes?.some((change) => change.blocking) ? (
          <section className="space-y-3">
            <p style={eyebrowStyle}>阻止应用的具体问题</p>
            {plan.changes.filter((change) => change.blocking).map((change, index) => (
              <div key={`${change.category}-${index}`} className="border-l-2 pl-3 text-sm space-y-1 break-words" style={{ ...sans, borderColor: "var(--danger)", color: "var(--fg)" }}>
                <p className="font-medium">{change.title}</p>
                {change.before ? <p className="text-xs break-all">{change.category === "executor" ? "节点" : "详情"}：{change.before}</p> : null}
                {change.after ? <p className="text-xs break-all">{change.category === "executor" ? "Pool" : "目标"}：{change.after}</p> : null}
                <p className="text-xs" style={{ color: "var(--muted)" }}>{change.impact}</p>
              </div>
            ))}
          </section>
        ) : null}
        {rollback ? <RollbackPanel rollback={rollback} /> : null}
      </div>
    );
  }

  return null;
}

function Placeholder({ text }: { text: string }) {
  return (
    <div className="py-8 text-center">
      <p className="pulse-soft text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
        {text}
      </p>
    </div>
  );
}

function PlanPreview({ plan }: { plan: DryRunResult }) {
  const [visibleCount, setVisibleCount] = useState(100);
  useEffect(() => setVisibleCount(100), [plan.id]);
  const visibleChanges = (plan.changes ?? []).slice(0, visibleCount);
  return (
    <div className="space-y-5 text-sm" style={{ ...sans, color: "var(--fg)" }}>
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <Cell label="目标网关模式" value={<span title={plan.gateway_mode}><Badge tone="accent">{gatewayModeLabel(plan.gateway_mode)}</Badge></span>} />
        <Cell label="新 spec_hash" value={<code style={{ fontFamily: "var(--font-mono)" }}>{plan.spec_hash.slice(0, 12)}…</code>} />
        <Cell label="dry-run id" value={<code style={{ fontFamily: "var(--font-mono)" }}>{plan.id}</code>} />
        <Cell label="config_epoch" value={`${plan.config_epoch}`} />
      </div>
      <section className="space-y-3">
        <div>
          <div className="text-xs font-semibold tracking-wide" style={{ color: "var(--fg)" }}>本次预演将发生的变化</div>
          <div className="mt-1 text-xs" style={{ color: "var(--muted)" }}>以下内容来自当前已应用配置与待应用配置的实际对比。</div>
        </div>
        <div className="space-y-2">
          {visibleChanges.map((change, index) => (
            <div key={`${change.category}-${change.title}-${index}`} className="px-4 py-3 min-w-0" style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}>
              <div className="flex items-start justify-between gap-3">
                <div>
                  <div className="text-[11px]" style={{ color: "var(--muted)" }}>{dryRunCategoryLabel(change.category)}</div>
                  <div className="mt-0.5 font-medium">{change.title}</div>
                </div>
                <Badge tone={change.blocking ? "err" : change.title.includes("没有实际配置变化") ? "muted" : "accent"}>{change.blocking ? "阻止应用" : change.title.includes("没有实际配置变化") ? "无变化" : "将变更"}</Badge>
              </div>
              {(change.before || change.after) && (
                <div className="mt-3 grid grid-cols-1 sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-center gap-2 text-xs">
                  <div className="px-2 py-1.5 min-w-0 break-words overflow-hidden" style={{ background: "var(--surface-1)", color: "var(--muted)" }}>{change.before || "无"}</div>
                  <span className="text-center rotate-90 sm:rotate-0" aria-hidden="true">→</span>
                  <div className="px-2 py-1.5 min-w-0 break-words overflow-hidden" style={{ background: "var(--surface-1)", color: "var(--fg)" }}>{change.after || "无"}</div>
                </div>
              )}
              <div className="mt-2 text-xs" style={{ color: "var(--muted)" }}>影响：{change.impact}</div>
            </div>
          ))}
          {plan.changes === undefined && <div className="px-4 py-3 text-xs" style={{ color: "var(--muted)", border: "1px solid var(--border)" }}>当前后端未提供可读变更摘要，请查看下方详细计划或刷新后重试。</div>}
          {plan.changes?.length === 0 && <div className="px-4 py-3 text-xs" style={{ color: "var(--muted)", border: "1px solid var(--border)" }}>未检测到可展示的配置差异。</div>}
          {plan.changes && visibleCount < plan.changes.length && (
            <Button variant="ghost" onClick={() => setVisibleCount((count) => Math.min(count + 100, plan.changes!.length))}>
              继续显示（剩余 {plan.changes.length - visibleCount} 项）
            </Button>
          )}
        </div>
      </section>
    </div>
  );
}

function DonePanel({ result }: { result: ApplyResult }) {
  const cores = useCores();
  const coreLabels = coreLabelMap(cores.data);
  return (
    <div className="space-y-4 text-sm" style={{ ...sans, color: "var(--fg)" }}>
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <Cell label="状态" value={<span title={result.status}><Badge tone="ok">{resultStatusLabel(result.status)}</Badge></span>} />
        <Cell label="config_epoch" value={`${result.config_epoch}`} />
        <Cell label="topology_hash" value={<code style={{ fontFamily: "var(--font-mono)" }}>{result.topology_hash.slice(0, 12)}…</code>} />
        <Cell
          label="Gateway Fabric"
          value={<Badge tone={result.fabric_applied ? "ok" : "muted"}>{result.fabric_applied ? "已应用" : "未应用"}</Badge>}
        />
        <Cell label="Workers" value={result.workers.map((worker) => `${worker.id}: ${applyStatusLabel(worker.status)}`).join("，") || "无"} />
        <Cell label="执行内核" value={result.core_results.map((core) => `${coreLabel(core.core, coreLabels)}: ${applyStatusLabel(core.status)}`).join("，") || "无"} />
      </div>
      {result.warnings && result.warnings.length > 0 ? (
        <div
          className="px-4 py-3"
          style={{
            background: "color-mix(in srgb, var(--warning) 10%, transparent)",
            border: "1px solid color-mix(in srgb, var(--warning) 35%, transparent)",
          }}
        >
          <p style={eyebrowStyle}>警告</p>
          <ul className="mt-2 list-disc pl-5 space-y-1">
            {result.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
}

function Cell({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div
      className="px-4 py-3"
      style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}
    >
      <p style={eyebrowStyle}>{label}</p>
      <div className="mt-1.5 text-[14px] break-all" style={{ color: "var(--fg-strong)" }}>
        {value}
      </div>
    </div>
  );
}

function Footer({
  step,
  plan,
  onClose,
  onStart,
  onApply,
  onDiscard,
}: {
  step: Step;
  plan: DryRunResult | null;
  onClose: () => void;
  onStart: () => void;
  onApply: () => void;
  onDiscard: () => void;
}) {
  if (step === "intro") {
    return (
      <>
        <Button variant="ghost" onClick={onDiscard}>放弃改动</Button>
        <Button variant="ghost" onClick={onClose}>稍后再说</Button>
        <Button onClick={onStart}>开始演练</Button>
      </>
    );
  }
  if (step === "checking" || step === "applying") {
    return <Button variant="ghost" disabled>处理中…</Button>;
  }
  if (step === "preview") {
    return (
      <>
        <Button variant="ghost" onClick={onClose}>取消</Button>
        <Button onClick={onApply} disabled={!plan || plan.changes === undefined || plan.changes_complete !== true || plan.changes.some((change) => change.blocking)}>确认应用</Button>
      </>
    );
  }
  if (step === "done") {
    return <Button onClick={onClose}>完成</Button>;
  }
  return (
    <>
      <Button variant="ghost" onClick={onClose}>关闭</Button>
      <Button onClick={onStart}>重试演练</Button>
    </>
  );
}

function RollbackPanel({ rollback }: { rollback: ApplyRollbackReport }) {
  const success = rollback.ok;
  const reasonText = rollback.reason
    ? errorText(new Error(rollback.reason), { fallback: "应用失败，已尝试自动回滚" })
    : "";
  const bg = success
    ? "color-mix(in srgb, var(--warning) 8%, transparent)"
    : "color-mix(in srgb, var(--danger) 12%, transparent)";
  const border = success
    ? "1px solid color-mix(in srgb, var(--warning) 35%, transparent)"
    : "1px solid color-mix(in srgb, var(--danger) 45%, transparent)";
  return (
    <div className="px-5 py-4 space-y-3" style={{ background: bg, border, borderRadius: "var(--radius-md)" }}>
      <div className="flex items-center gap-2">
        <Badge tone={success ? "warn" : "err"}>{success ? "已回滚" : "回滚异常"}</Badge>
        <p className="text-sm" style={{ ...sans, color: "var(--fg-strong)" }}>
          {success
            ? "应用失败，已自动回滚到上一次运行配置。"
            : "应用失败，自动回滚未完全成功，请切换至 safe/off 模式或联系后端处理。"}
        </p>
      </div>
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 text-xs" style={{ ...sans, color: "var(--fg)" }}>
        {rollback.stage ? (
          <div>
            <span style={{ color: "var(--muted)" }}>失败阶段：</span>
            <span title={rollback.stage}>{rollbackStageLabel(rollback.stage)}</span>
          </div>
        ) : null}
        {rollback.reason ? (
          <div>
            <span style={{ color: "var(--muted)" }}>原因：</span>
            <span title={rollback.reason}>
              {reasonText}
              {reasonText !== rollback.reason ? (
                <span className="mt-1 block break-words" style={{ color: "var(--muted)" }}>
                  诊断：{rollback.reason}
                </span>
              ) : null}
            </span>
          </div>
        ) : null}
        <div>
          <span style={{ color: "var(--muted)" }}>Worker 集已恢复：</span>
          <Badge tone={rollback.workers_restored ? "ok" : "err"}>{rollback.workers_restored ? "是" : "否"}</Badge>
        </div>
        <div>
          <span style={{ color: "var(--muted)" }}>Gateway Fabric 已恢复：</span>
          <Badge tone={rollback.fabric_restored ? "ok" : "err"}>{rollback.fabric_restored ? "是" : "否"}</Badge>
        </div>
        <div>
          <span style={{ color: "var(--muted)" }}>Binding 已恢复：</span>
          <Badge tone={rollback.bindings_restored ? "ok" : "err"}>{rollback.bindings_restored ? "是" : "否"}</Badge>
        </div>
        <div>
          <span style={{ color: "var(--muted)" }}>持久 Selection 已恢复：</span>
          <Badge tone={rollback.selection_restored ? "ok" : "err"}>{rollback.selection_restored ? "是" : "否"}</Badge>
        </div>
      </div>
      {rollback.errors && rollback.errors.length > 0 ? (
        <details className="text-xs" style={{ color: "var(--muted)" }}>
          <summary className="cursor-pointer">回滚过程中的错误 ({rollback.errors.length})</summary>
          <ul className="mt-1 list-disc pl-5 space-y-0.5">
            {rollback.errors.map((err, i) => <li key={i} style={{ color: "var(--danger)" }}>{err}</li>)}
          </ul>
        </details>
      ) : null}
    </div>
  );
}
