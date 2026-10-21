import { useState } from "react";
import {
  useScheduler,
  useSchedulerTaskCreate,
  useSchedulerTaskUpdate,
  useSchedulerTaskDelete,
  useSchedulerTaskRun,
} from "../lib/queries";
import type { SchedulerTask } from "../lib/types";
import { Card, SectionHeader } from "../ui/Card";
import { Badge, Dot } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Field, Input, Select } from "../ui/Field";
import { Toggle } from "../ui/Segmented";
import { Modal } from "../ui/Modal";
import { serif, sans } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

const TASK_TYPES: { value: string; label: string }[] = [
  { value: "provider_refresh", label: "刷新订阅" },
  { value: "delay_test", label: "延迟测试" },
  { value: "failover_watchdog", label: "故障切换看门狗" },
  { value: "backup", label: "自动备份" },
  { value: "apply_config", label: "应用配置" },
  { value: "restart_service", label: "重启服务" },
  { value: "reboot_router", label: "重启路由器" },
  { value: "custom_shell", label: "自定义脚本" },
];

const TYPE_LABEL: Record<string, string> = Object.fromEntries(
  TASK_TYPES.map((t) => [t.value, t.label])
);

const STATUS_LABEL: Record<string, string> = {
  ok: "成功",
  degraded: "部分成功",
  failed: "失败",
  running: "执行中",
  queued: "已排队",
  locked: "已锁定",
  unsupported: "不支持",
  stub: "桩（Mac）",
};

export function SchedulerPage() {
  const scheduler = useScheduler();
  const create = useSchedulerTaskCreate();
  const update = useSchedulerTaskUpdate();
  const del = useSchedulerTaskDelete();
  const run = useSchedulerTaskRun();
  const [createModal, setCreateModal] = useState(false);
  const [editTarget, setEditTarget] = useState<SchedulerTask | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<SchedulerTask | null>(null);
  const [confirmRunTarget, setConfirmRunTarget] = useState<SchedulerTask | null>(null);
  const [error, setError] = useState<unknown>(null);

  const tasks = scheduler.data?.tasks ?? [];

  async function handleRun(task: SchedulerTask) {
    if (task.type === "reboot_router") {
      setConfirmRunTarget(task);
      return;
    }
    setError(null);
    try {
      await run.mutateAsync({ id: task.id });
    } catch (e) {
      setError(e);
    }
  }

  async function handleConfirmRun() {
    if (!confirmRunTarget) return;
    setError(null);
    try {
      await run.mutateAsync({ id: confirmRunTarget.id, confirm: true });
      setConfirmRunTarget(null);
    } catch (e) {
      setError(e);
    }
  }

  async function handleToggle(task: SchedulerTask) {
    setError(null);
    try {
      await update.mutateAsync({ id: task.id, task: { enabled: !task.enabled } });
    } catch (e) {
      setError(e);
    }
  }

  async function handleDelete() {
    if (!deleteTarget) return;
    setError(null);
    try {
      await del.mutateAsync(deleteTarget.id);
      setDeleteTarget(null);
    } catch (e) {
      setError(e);
    }
  }

  return (
    <div className="space-y-12 max-w-[1120px]">
      <SectionHeader
        eyebrow="计划任务 · Scheduler"
        title="定时与手动任务管理"
        desc="查看后台自动任务、手动执行、编辑定时调度。"
        right={
          <div className="flex items-center gap-3">
            {scheduler.isLoading ? <Badge tone="muted">载入中…</Badge> : null}
            <Button onClick={() => setCreateModal(true)}>新增任务</Button>
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
            <ErrorText error={error} fallback="计划任务操作失败" showDiagnostic />
          </p>
        </div>
      ) : null}

      <Card>
        {tasks.length === 0 ? (
          <div className="px-7 py-10 text-center" style={{ ...sans, color: "var(--muted)" }}>
            暂无计划任务。
          </div>
        ) : (
          <ul>
            {tasks.map((t, i) => (
              <li
                key={t.id}
                className="px-7 py-5"
                style={{ borderBottom: i === tasks.length - 1 ? "none" : "1px solid var(--border)" }}
              >
                <div className="flex items-start justify-between gap-5">
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2 flex-wrap">
                      <p className="text-[18px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                        {t.name || t.id}
                      </p>
                      <Badge tone={t.enabled ? "ok" : "muted"}>
                        <Dot tone={t.enabled ? "ok" : "muted"} />
                        {t.enabled ? "启用" : "禁用"}
                      </Badge>
                      <Badge tone="accent">{TYPE_LABEL[t.type] || t.type}</Badge>
                      {t.running ? <Badge tone="warn">执行中</Badge> : null}
                    </div>
                    <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
                      <code style={{ fontFamily: "var(--font-mono)" }}>{t.id}</code> · 调度：
                      <code style={{ fontFamily: "var(--font-mono)" }}>{t.schedule}</code>
                    </p>
                    <div className="mt-2 grid sm:grid-cols-3 gap-2 text-xs" style={{ ...sans, color: "var(--muted)" }}>
                      <span>
                        上次运行：{t.last_run ? new Date(t.last_run).toLocaleString() : "—"}
                      </span>
                      <span>
                        结果：<span title={t.last_status}>{t.last_status ? STATUS_LABEL[t.last_status] || "状态未知" : "—"}</span>
                      </span>
                      <span>
                        下次运行：{t.next_run ? new Date(t.next_run).toLocaleString() : "—"}
                      </span>
                    </div>
                    {t.last_output ? (
                      <p className="mt-1 text-[11px] italic" style={{ ...serif, color: "var(--muted)" }}>
                        输出：{t.last_output}
                      </p>
                    ) : null}
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    <Toggle on={t.enabled} onChange={() => handleToggle(t)} />
                    <Button size="sm" variant="ghost" onClick={() => handleRun(t)} loading={run.isPending} disabled={t.running}>
                      运行
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setEditTarget(t)}>
                      编辑
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setDeleteTarget(t)}>
                      删除
                    </Button>
                  </div>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>

      <TaskFormModal
        open={createModal}
        title="新增任务"
        onClose={() => setCreateModal(false)}
        onSubmit={async (t) => {
          setError(null);
          try {
            await create.mutateAsync(t);
            setCreateModal(false);
          } catch (e) {
            setError(e);
          }
        }}
        loading={create.isPending}
      />

      <TaskFormModal
        open={!!editTarget}
        title={`编辑 ${editTarget?.name || editTarget?.id || ""}`}
        initial={editTarget}
        onClose={() => setEditTarget(null)}
        onSubmit={async (t) => {
          if (!editTarget) return;
          setError(null);
          try {
            await update.mutateAsync({ id: editTarget.id, task: t });
            setEditTarget(null);
          } catch (e) {
            setError(e);
          }
        }}
        loading={update.isPending}
      />

      <Modal
        open={!!deleteTarget}
        onClose={() => setDeleteTarget(null)}
        eyebrow="确认删除"
        title={`删除任务 ${deleteTarget?.name || deleteTarget?.id || ""}?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDeleteTarget(null)}>取消</Button>
            <Button variant="danger" onClick={handleDelete} loading={del.isPending}>删除</Button>
          </>
        }
      >
        <p className="text-sm leading-7" style={{ ...sans, color: "var(--fg)" }}>
          删除后任务及其运行历史将永久丢失。
        </p>
      </Modal>

      <Modal
        open={!!confirmRunTarget}
        onClose={() => setConfirmRunTarget(null)}
        eyebrow="确认执行"
        title="手动执行「重启路由器」？"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmRunTarget(null)}>取消</Button>
            <Button variant="danger" onClick={handleConfirmRun} loading={run.isPending}>确认重启</Button>
          </>
        }
      >
        <p className="text-sm leading-7" style={{ ...sans, color: "var(--fg)" }}>
          手动执行「重启路由器」会立即重启路由器，所有连接中断。
        </p>
      </Modal>
    </div>
  );
}

function TaskFormModal({
  open,
  title,
  initial,
  onClose,
  onSubmit,
  loading,
}: {
  open: boolean;
  title: string;
  initial?: SchedulerTask | null;
  onClose: () => void;
  onSubmit: (task: Pick<SchedulerTask, "id" | "name" | "type" | "enabled" | "schedule" | "spec">) => void;
  loading: boolean;
}) {
  const [form, setForm] = useState({
    id: initial?.id || "",
    name: initial?.name || "",
    type: initial?.type || "provider_refresh",
    enabled: initial?.enabled ?? true,
    schedule: initial?.schedule || "@every 4h",
    service: typeof initial?.spec?.service === "string" ? initial.spec.service : "",
  });

  function submit() {
    if (!form.id.trim() || !form.name.trim() || !form.schedule.trim()) return;
    if (form.type === "restart_service" && !form.service.trim()) return;
    onSubmit({
      id: form.id.trim(),
      name: form.name.trim(),
      type: form.type,
      enabled: form.enabled,
      schedule: form.schedule.trim(),
      spec: form.type === "restart_service" ? { service: form.service.trim() } : undefined,
    });
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow="计划任务"
      title={title}
      width={640}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button onClick={submit} loading={loading}>保存</Button>
        </>
      }
    >
      <div className="space-y-5">
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Field label="任务 ID">
            <Input
              value={form.id}
              onChange={(e) => setForm({ ...form, id: e.target.value })}
              disabled={!!initial}
              placeholder="provider-auto-refresh"
            />
          </Field>
          <Field label="名称">
            <Input
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              placeholder="Provider auto refresh"
            />
          </Field>
        </div>
        <Field label="类型">
          <Select value={form.type} onChange={(e) => setForm({ ...form, type: e.target.value })}>
            {TASK_TYPES.map((t) => (
              <option key={t.value} value={t.value}>{t.label}（{t.value}）</option>
            ))}
          </Select>
        </Field>
        {form.type === "restart_service" ? (
          <Field label="服务名" hint="由后端服务白名单验证">
            <Input
              value={form.service}
              onChange={(e) => setForm({ ...form, service: e.target.value })}
              placeholder="smartgate"
            />
          </Field>
        ) : null}
        <Field label="调度表达式" hint="支持 cron（0 5 * * *）或 @every 4h 语法">
          <Input
            value={form.schedule}
            onChange={(e) => setForm({ ...form, schedule: e.target.value })}
            placeholder="@every 4h / 0 5 * * *"
          />
        </Field>
        <div className="flex items-center justify-between">
          <span className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
            启用
          </span>
          <Toggle on={form.enabled} onChange={(v) => setForm({ ...form, enabled: v })} />
        </div>
      </div>
    </Modal>
  );
}
