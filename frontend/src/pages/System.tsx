import { NetworkRecovery } from "./NetworkRecovery";
import { useState } from "react";
import {
  useCores,
  useSystemStatus,
  useSystemLogs,
  useSystemServiceCommand,
  useSystemDnsFlush,
  useSystemReboot,
} from "../lib/queries";
import type { SystemServiceName, SystemServiceAction } from "../lib/types";
import { Card, SectionHeader, Eyebrow, Rule } from "../ui/Card";
import { Badge, Dot } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Field, Select } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { runtimePhaseLabel, serviceActionLabel, serviceStatusLabel, systemLogsResultLabel } from "../lib/statusText";

export function SystemPage() {
  const status = useSystemStatus();
  const serviceCmd = useSystemServiceCommand();
  const dnsFlush = useSystemDnsFlush();
  const reboot = useSystemReboot();
  const [confirmModal, setConfirmModal] = useState<{
    action: "service" | "dns" | "reboot";
    service?: SystemServiceName;
    serviceAction?: SystemServiceAction;
  } | null>(null);
  const [error, setError] = useState<unknown>(null);

  async function executeConfirmedAction() {
    if (!confirmModal) return;
    setError(null);
    try {
      if (confirmModal.action === "service" && confirmModal.service && confirmModal.serviceAction) {
        await serviceCmd.mutateAsync({
          service: confirmModal.service,
          action: confirmModal.serviceAction,
        });
      } else if (confirmModal.action === "dns") {
        await dnsFlush.mutateAsync();
      } else if (confirmModal.action === "reboot") {
        await reboot.mutateAsync();
      }
      setConfirmModal(null);
    } catch (e) {
      setError(e);
    }
  }

  const data = status.data;

  return (
    <div className="space-y-12 max-w-[1120px]">
      <NetworkRecovery />
      <SectionHeader
        eyebrow="系统 · System"
        title="路由器状态与管理"
        desc="查看内存、磁盘、服务、进程状态；执行服务重启、DNS flush、路由器重启等操作。"
        right={
          status.isLoading ? (
            <Badge tone="muted">载入中…</Badge>
          ) : status.error ? (
            <Badge tone="err"><ErrorText error={status.error} fallback="系统状态加载失败" /></Badge>
          ) : null
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
            <ErrorText error={error} fallback="系统操作失败" showDiagnostic />
          </p>
        </div>
      ) : null}

      {data ? (
        <>
          <section className="grid lg:grid-cols-2 gap-6">
            <Card inset>
              <Eyebrow>运行时</Eyebrow>
              <div className="mt-4 space-y-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
                <Row
                  label="阶段"
                  value={
                    <span title={data.runtime.phase}>
                      <Badge tone={runtimePhaseTone(data.runtime.phase)}>{runtimePhaseLabel(data.runtime.phase)}</Badge>
                    </span>
                  }
                />
                <Row label="监听地址" value={data.runtime.listen_addr} />
                <Row label="config_epoch" value={data.runtime.config_epoch} />
                <Row
                  label="救援模式"
                  value={
                    data.runtime.rescue_active ? (
                      <span className="space-y-1 block">
                        <Badge tone="warn">已激活</Badge>
                        <span className="block text-xs" style={{ color: "var(--muted)" }}>代理已全部停止、全屋直连；恢复代理请清除救援状态</span>
                      </span>
                    ) : (
                      "—"
                    )
                  }
                />
              </div>
            </Card>
            {data.memory ? (
              <Card inset>
                <Eyebrow>内存</Eyebrow>
                <div className="mt-4 space-y-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
                  <Row label="总计" value={formatBytes(data.memory.total_bytes)} />
                  <Row label="已用" value={formatBytes(data.memory.used_bytes)} />
                  <Row label="可用" value={formatBytes(data.memory.available_bytes)} />
                  <Row label="空闲" value={formatBytes(data.memory.free_bytes)} />
                </div>
              </Card>
            ) : null}
          </section>

          {data.disks.length > 0 ? (
            <section>
              <SectionHeader eyebrow="磁盘" title={`共 ${data.disks.length} 个挂载点`} />
              <Card>
                <ul>
                  {data.disks.map((d, i) => (
                    <li
                      key={d.path}
                      className="px-7 py-5 flex items-start justify-between gap-5"
                      style={{ borderBottom: i === data.disks.length - 1 ? "none" : "1px solid var(--border)" }}
                    >
                      <div className="flex-1 min-w-0">
                        <p className="text-[18px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                          {d.path}
                        </p>
                        <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
                          {formatBytes(d.used_bytes)} / {formatBytes(d.total_bytes)} ·{" "}
                          {d.used_percent.toFixed(1)}% 已用
                        </p>
                      </div>
                      <Badge tone={d.used_percent > 85 ? "warn" : "muted"}>
                        {formatBytes(d.free_bytes)} 可用
                      </Badge>
                    </li>
                  ))}
                </ul>
              </Card>
            </section>
          ) : null}

          {data.services.length > 0 ? (
            <section>
              <SectionHeader eyebrow="服务" title="OpenWrt 服务状态" />
              <Card>
                <ul>
                  {data.services.map((s, i) => (
                    <li
                      key={s.name}
                      className="px-7 py-5 flex items-start justify-between gap-5"
                      style={{ borderBottom: i === data.services.length - 1 ? "none" : "1px solid var(--border)" }}
                    >
                      <div className="flex-1 min-w-0">
                        <div className="flex items-center gap-2">
                          <p className="text-[18px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                            {s.name}
                          </p>
                          <span title={s.status}>
                            <Badge tone={serviceStatusTone(s.status)}>
                              <Dot tone={serviceStatusTone(s.status)} />
                              {serviceStatusLabel(s.status)}
                            </Badge>
                          </span>
                        </div>
                        {s.result?.output || s.result?.error ? <CommandDiagnostic result={s.result} /> : null}
                      </div>
                      <div className="flex items-center gap-2">
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() =>
                            setConfirmModal({
                              action: "service",
                              service: s.name as SystemServiceName,
                              serviceAction: "restart",
                            })
                          }
                        >
                          重启
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() =>
                            setConfirmModal({
                              action: "service",
                              service: s.name as SystemServiceName,
                              serviceAction: "reload",
                            })
                          }
                        >
                          重载
                        </Button>
                      </div>
                    </li>
                  ))}
                </ul>
              </Card>
            </section>
          ) : null}

          {data.processes.length > 0 ? (
            <section>
              <SectionHeader eyebrow="进程" title="关键进程状态" />
              <Card>
                <ul>
                  {data.processes.map((p, i) => (
                    <li
                      key={p.name}
                      className="px-7 py-4 flex items-start justify-between gap-5"
                      style={{ borderBottom: i === data.processes.length - 1 ? "none" : "1px solid var(--border)" }}
                    >
                      <div className="flex-1 min-w-0">
                        <div className="flex items-center gap-2">
                          <p className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                            {p.name}
                          </p>
                          <Badge tone={p.running ? "ok" : "err"}>
                            {p.running ? "运行中" : "未运行"}
                          </Badge>
                        </div>
                        {p.pids.length > 0 ? (
                          <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
                            PID: {p.pids.join(", ")}
                          </p>
                        ) : null}
                      </div>
                    </li>
                  ))}
                </ul>
              </Card>
            </section>
          ) : null}

          {data.warnings.length > 0 ? (
            <div
              className="px-5 py-3"
              style={{
                background: "color-mix(in srgb, var(--warning) 12%, transparent)",
                border: "1px solid color-mix(in srgb, var(--warning) 35%, transparent)",
              }}
            >
              <p style={eyebrowStyle}>警告</p>
              <ul className="mt-2 space-y-1 text-sm" style={{ ...sans, color: "var(--fg)" }}>
                {data.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </div>
          ) : null}

          <Rule glyph="MGMT" />

          <section className="space-y-5">
            <SectionHeader eyebrow="管理" title="破坏性操作" desc="需二次确认的系统级操作。" />
            <div className="flex flex-wrap items-center gap-3">
              <Button
                variant="ghost"
                onClick={() => setConfirmModal({ action: "dns" })}
                loading={dnsFlush.isPending}
              >
                Flush DNS 缓存
              </Button>
              <Button
                variant="danger"
                onClick={() => setConfirmModal({ action: "reboot" })}
                loading={reboot.isPending}
              >
                重启路由器
              </Button>
            </div>
          </section>

          <LogsSection />
        </>
      ) : null}

      <Modal
        open={!!confirmModal}
        onClose={() => setConfirmModal(null)}
        eyebrow="确认操作"
        title={confirmTitleOf(confirmModal)}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmModal(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              onClick={executeConfirmedAction}
              loading={serviceCmd.isPending || dnsFlush.isPending || reboot.isPending}
            >
              确认执行
            </Button>
          </>
        }
      >
        <p className="text-sm leading-7" style={{ ...sans, color: "var(--fg)" }}>
          {confirmDescOf(confirmModal)}
        </p>
      </Modal>
    </div>
  );
}

function runtimePhaseTone(phase: string): "ok" | "warn" | "err" | "info" | "muted" {
  if (phase === "running") return "ok";
  if (phase === "rescue") return "warn";
  if (phase === "stopped") return "err";
  if (phase === "starting") return "info";
  return "muted";
}

function serviceStatusTone(status: string): "ok" | "warn" | "err" | "muted" {
  if (status === "running") return "ok";
  if (status === "stub") return "muted";
  if (status === "not_running" || status === "failed") return "err";
  return "warn";
}

function CommandDiagnostic({ result }: { result: { output?: string; error?: string } }) {
  const content = [result.output, result.error].filter(Boolean).join("\n");
  return (
    <details className="mt-2 text-xs" style={{ ...sans, color: "var(--muted)" }}>
      <summary className="cursor-pointer select-none">{result.error ? "诊断信息" : "命令输出"}</summary>
      <pre
        className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-words"
        style={{ fontFamily: "var(--font-mono)", color: "var(--fg)" }}
      >
        {content}
      </pre>
    </details>
  );
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <span style={{ color: "var(--muted)" }}>{label}</span>
      <span style={{ color: "var(--fg-strong)" }}>{value}</span>
    </div>
  );
}

function formatBytes(b: number): string {
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  if (b < 1024 * 1024 * 1024) return `${(b / (1024 * 1024)).toFixed(1)} MB`;
  return `${(b / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

function confirmTitleOf(modal: { action: string; service?: string; serviceAction?: string } | null): string {
  if (!modal) return "";
  if (modal.action === "service") return `${serviceActionLabel(modal.serviceAction)} ${modal.service}？`;
  if (modal.action === "dns") return "Flush DNS 缓存?";
  if (modal.action === "reboot") return "重启路由器?";
  return "确认操作";
}

function confirmDescOf(modal: { action: string; service?: string; serviceAction?: string } | null): string {
  if (!modal) return "";
  if (modal.action === "service") {
    return `即将执行 /etc/init.d/${modal.service} ${modal.serviceAction}。该服务会短暂中断。`;
  }
  if (modal.action === "dns") {
    return "即将重启 dnsmasq 以清空 DNS 缓存。连接会短暂中断。";
  }
  if (modal.action === "reboot") {
    return "即将执行 reboot。路由器将完全重启，所有连接中断，约 1-2 分钟后恢复。";
  }
  return "";
}

function LogsSection() {
  const cores = useCores();
  const [source, setSource] = useState<string>("system");
  const logs = useSystemLogs(source, 100);

  return (
    <section className="space-y-5">
      <SectionHeader eyebrow="日志" title="系统日志查看" />
      <div className="flex items-center gap-3">
        <Field label="来源">
          <Select value={source} onChange={(e) => setSource(e.target.value)}>
            <option value="system">system</option>
            <option value="smartgate">smartgate</option>
            <option value="dnsmasq">dnsmasq</option>
            {cores.data?.map((core) => (
              <option key={core.id} value={core.id}>{core.display_name || core.id}</option>
            ))}
          </Select>
        </Field>
        {logs.isLoading ? <Badge tone="muted">载入中…</Badge> : null}
      </div>
      {logs.data ? (
        <Card>
          <div className="px-7 py-5">
            <p style={eyebrowStyle}>
              {logs.data.source} · {logs.data.lines.length} 条 · 日志状态：
              <span title={logs.data.result}>{systemLogsResultLabel(logs.data.result)}</span>
            </p>
            <pre
              className="mt-4 text-[11px] leading-5 max-h-96 overflow-auto"
              style={{ fontFamily: "var(--font-mono)", color: "var(--fg)" }}
            >
              {logs.data.lines.join("\n")}
            </pre>
          </div>
        </Card>
      ) : null}
    </section>
  );
}
