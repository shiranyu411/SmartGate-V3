import { useState } from "react";
import {
  useSnapshots,
  useSnapshotCreate,
  useSnapshotRollbackDryrun,
  useSnapshotRollbackApply,
} from "../lib/queries";
import type { Snapshot, DryRunResult, ApplyResult } from "../lib/types";
import { Card, SectionHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Field, Input, Select } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { makeOpId } from "../lib/api";
import { rollbackModalStateKey } from "./rollbackState";
import { dryRunCategoryLabel, snapshotKindLabel, snapshotResultStatusLabel } from "../lib/statusText";
import { gatewayModeLabel } from "./gatewayState";
import { ErrorText } from "../ui/ErrorText";

type RollbackStep = "intro" | "dryrun" | "preview" | "applying" | "done" | "error";

export function SnapshotsPage() {
  const snapshots = useSnapshots();
  const create = useSnapshotCreate();
  const [createModal, setCreateModal] = useState(false);
  const [rollbackTarget, setRollbackTarget] = useState<Snapshot | null>(null);
  const [error, setError] = useState<unknown>(null);

  const list = snapshots.data?.snapshots ?? [];

  return (
    <div className="space-y-12 max-w-[1120px]">
      <SectionHeader
        eyebrow="快照 · Snapshots"
        title="配置快照与回滚"
        desc="快照捕获当前 spec、runtime 元数据、last-good 节点，用于回滚。不含 tar 备份，完整归档请用 Backups。"
        right={
          <div className="flex items-center gap-3">
            {snapshots.isLoading ? <Badge tone="muted">载入中…</Badge> : null}
            <Button onClick={() => setCreateModal(true)}>新建快照</Button>
          </div>
        }
      />

      {error ? (
        <div
          className="px-5 py-3"
          style={{
            background: "color-mix(in srgb, var(--danger) 8%, transparent)",
            border: "1px solid color-mix(in srgb, var(--danger) 35%, transparent)",
          }}
        >
          <p className="break-words text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={error} fallback="快照操作失败" showDiagnostic />
          </p>
        </div>
      ) : null}

      <Card>
        {list.length === 0 ? (
          <div className="px-7 py-10 text-center" style={{ ...sans, color: "var(--muted)" }}>
            暂无快照。
          </div>
        ) : (
          <ul>
            {list.map((s, i) => (
              <li
                key={s.id}
                className="px-7 py-5 flex items-start justify-between gap-5"
                style={{ borderBottom: i === list.length - 1 ? "none" : "1px solid var(--border)" }}
              >
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2 flex-wrap">
                    <p className="text-[18px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                      {s.description || s.id}
                    </p>
                    <span title={s.kind}>
                      <Badge tone={s.kind === "manual" ? "accent" : "muted"}>{snapshotKindLabel(s.kind)}</Badge>
                    </span>
                    {s.last_good_captured ? <Badge tone="ok">含 last-good</Badge> : null}
                  </div>
                  <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
                    <code style={{ fontFamily: "var(--font-mono)" }}>{s.id}</code> ·{" "}
                    {new Date(s.saved_at).toLocaleString()} · {s.provider_count} 订阅 ·{" "}
                    {s.business_pool_count} 池 · {s.last_good_count} 节点
                  </p>
                </div>
                <Button size="sm" onClick={() => setRollbackTarget(s)}>
                  回滚到此
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Card>

      <CreateModal
        open={createModal}
        onClose={() => setCreateModal(false)}
        onCreate={async (desc, kind) => {
          setError(null);
          try {
            await create.mutateAsync({ description: desc, kind });
            setCreateModal(false);
          } catch (e) {
            setError(e);
          }
        }}
        loading={create.isPending}
      />

      <RollbackModal
        key={rollbackModalStateKey(!!rollbackTarget, rollbackTarget?.id)}
        open={!!rollbackTarget}
        snapshot={rollbackTarget}
        onClose={() => setRollbackTarget(null)}
      />
    </div>
  );
}

function CreateModal({
  open,
  onClose,
  onCreate,
  loading,
}: {
  open: boolean;
  onClose: () => void;
  onCreate: (desc: string, kind: string) => void;
  loading: boolean;
}) {
  const [desc, setDesc] = useState("");
  const [kind, setKind] = useState("manual");

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="新建快照"
      title="捕获当前配置状态"
      width={560}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            取消
          </Button>
          <Button onClick={() => onCreate(desc, kind)} loading={loading}>
            创建
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <Field label="描述" hint="方便将来识别">
          <Input
            value={desc}
            onChange={(e) => setDesc(e.target.value)}
            placeholder="例如：编辑业务池之前"
          />
        </Field>
        <Field label="类型">
          <Select value={kind} onChange={(e) => setKind(e.target.value)}>
            <option value="manual">手动</option>
            <option value="scheduled">定时</option>
            <option value="before-edit">编辑前</option>
          </Select>
        </Field>
      </div>
    </Modal>
  );
}

function RollbackModal({
  open,
  snapshot,
  onClose,
}: {
  open: boolean;
  snapshot: Snapshot | null;
  onClose: () => void;
}) {
  const dryrun = useSnapshotRollbackDryrun();
  const apply = useSnapshotRollbackApply();
  const [step, setStep] = useState<RollbackStep>("intro");
  const [plan, setPlan] = useState<DryRunResult | null>(null);
  const [result, setResult] = useState<ApplyResult | null>(null);
  const [error, setError] = useState<unknown>(null);
	  const [dryrunId] = useState(() => makeOpId("rollback"));

  async function runDryrun() {
    if (!snapshot) return;
    setError(null);
    setStep("dryrun");
    try {
      const data = await dryrun.mutateAsync({ dryrunId, snapshotId: snapshot.id });
      setPlan(data);
      setStep("preview");
    } catch (e) {
      setError(e);
      setStep("error");
    }
  }

  async function runApply() {
    if (!snapshot || !plan) return;
    setStep("applying");
    setError(null);
    try {
      const data = await apply.mutateAsync({ dryrunId: plan.id, snapshotId: snapshot.id });
      setResult(data);
      setStep("done");
    } catch (e) {
      setError(e);
      setStep("error");
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="回滚快照"
      title={titleOf(step)}
      width={720}
      footer={
        <Footer
          step={step}
          onClose={onClose}
          onStart={runDryrun}
          onApply={runApply}
          plan={plan}
        />
      }
    >
      <Body step={step} snapshot={snapshot} plan={plan} result={result} error={error} />
    </Modal>
  );
}

function titleOf(step: RollbackStep): string {
  switch (step) {
    case "intro":
      return "确认回滚";
    case "dryrun":
      return "正在演练回滚计划";
    case "preview":
      return "请确认即将回滚的内容";
    case "applying":
      return "正在回滚配置";
    case "done":
      return "回滚完成";
    case "error":
      return "出现问题";
  }
}

function Body({
  step,
  snapshot,
  plan,
  result,
  error,
}: {
  step: RollbackStep;
  snapshot: Snapshot | null;
  plan: DryRunResult | null;
  result: ApplyResult | null;
  error: unknown;
}) {
  const [visibleChanges, setVisibleChanges] = useState(100);
  if (step === "intro" && snapshot) {
    return (
      <div className="space-y-4">
        <p className="text-[15px] leading-7" style={{ ...serif, color: "var(--fg)" }}>
          即将回滚到快照 <strong>{snapshot.description || snapshot.id}</strong>，保存于{" "}
          {new Date(snapshot.saved_at).toLocaleString()}。
        </p>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 text-sm" style={{ ...sans, color: "var(--fg)" }}>
          <Cell label="快照 ID" value={snapshot.id} />
          <Cell label="类型" value={<span title={snapshot.kind}>{snapshotKindLabel(snapshot.kind)}</span>} />
          <Cell label="订阅" value={snapshot.provider_count} />
          <Cell label="业务池" value={snapshot.business_pool_count} />
          <Cell label="last-good 节点" value={snapshot.last_good_count} />
        </div>
        <p className="text-xs" style={{ ...sans, color: "var(--muted)" }}>
          回滚会恢复 spec 与 last-good 节点，并执行 dryrun → apply 流程。当前未提交的改动会丢失。
        </p>
      </div>
    );
  }

  if (step === "dryrun") {
    return <Placeholder text="正在生成回滚计划并校验候选配置…" />;
  }

  if (step === "applying") {
    return <Placeholder text="正在落地回滚配置…" />;
  }

  if (step === "preview" && plan) {
    return (
      <div className="space-y-5 text-sm" style={{ ...sans, color: "var(--fg)" }}>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Cell label="目标网关模式" value={<span title={plan.gateway_mode}><Badge tone="accent">{gatewayModeLabel(plan.gateway_mode)}</Badge></span>} />
          <Cell label="config_epoch" value={`${plan.config_epoch}`} />
        </div>
        <div className="space-y-2">
          <div className="text-xs font-semibold">本次回滚将发生的变化</div>
          {(plan.changes ?? []).slice(0, visibleChanges).map((change, index) => (
            <div key={`${change.category}-${change.title}-${index}`} className="px-4 py-3 min-w-0" style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}>
              <div className="text-[11px]" style={{ color: "var(--muted)" }}>{dryRunCategoryLabel(change.category)}</div>
              <div className="font-medium">{change.title}</div>
              <div className="mt-2 grid grid-cols-1 sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] gap-2 text-xs">
                <div className="break-words">{change.before || "无"}</div><span className="text-center">→</span><div className="break-words">{change.after || "无"}</div>
              </div>
              <div className="mt-2 text-xs" style={{ color: "var(--muted)" }}>影响：{change.impact}</div>
            </div>
          ))}
          {plan.changes === undefined && <div className="text-xs" style={{ color: "var(--danger)" }}>后端未提供可读摘要，禁止回滚。</div>}
          {plan.changes && plan.changes.length > visibleChanges && (
            <Button size="sm" variant="ghost" onClick={() => setVisibleChanges((count) => count + 100)}>
              再显示 100 项（剩余 {plan.changes.length - visibleChanges} 项）
            </Button>
          )}
        </div>
      </div>
    );
  }

  if (step === "done" && result) {
    return (
      <div className="space-y-4 text-sm" style={{ ...sans, color: "var(--fg)" }}>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Cell label="状态" value={<span title={result.status}><Badge tone="ok">{snapshotResultStatusLabel(result.status)}</Badge></span>} />
          <Cell label="config_epoch" value={`${result.config_epoch}`} />
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

  if (step === "error") {
    return (
      <div
        className="px-5 py-4"
        style={{
          background: "color-mix(in srgb, var(--danger) 8%, transparent)",
          border: "1px solid color-mix(in srgb, var(--danger) 35%, transparent)",
        }}
      >
        <p style={eyebrowStyle}>错误</p>
        <p className="break-words mt-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
          <ErrorText error={error} fallback="快照回滚失败" showDiagnostic />
        </p>
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
}: {
  step: RollbackStep;
  plan: DryRunResult | null;
  onClose: () => void;
  onStart: () => void;
  onApply: () => void;
}) {
  if (step === "intro") {
    return (
      <>
        <Button variant="ghost" onClick={onClose}>
          取消
        </Button>
        <Button onClick={onStart}>开始演练</Button>
      </>
    );
  }
  if (step === "dryrun" || step === "applying") {
    return <Button variant="ghost" disabled>处理中…</Button>;
  }
  if (step === "preview") {
    return (
      <>
        <Button variant="ghost" onClick={onClose}>
          取消
        </Button>
        <Button onClick={onApply} disabled={!plan || plan.changes === undefined || plan.changes_complete !== true}>
          确认回滚
        </Button>
      </>
    );
  }
  if (step === "done") {
    return <Button onClick={onClose}>完成</Button>;
  }
  return (
    <>
      <Button variant="ghost" onClick={onClose}>
        关闭
      </Button>
      <Button onClick={onStart}>重试演练</Button>
    </>
  );
}
