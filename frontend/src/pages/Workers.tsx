import { useState } from "react";
import { useCores, useWorkerEvents, useWorkers } from "../lib/queries";
import type { WorkerEvent, WorkerState, WorkerStatus } from "../lib/types";
import { coreLabel, coreLabelMap, type CoreLabelMap } from "../lib/coreLabels";
import { Badge, Dot } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Card, Eyebrow, Rule, SectionHeader } from "../ui/Card";
import { Modal } from "../ui/Modal";
import { eyebrowStyle, sans, serif } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { workerEventKindLabel } from "../lib/statusText";

type BadgeTone = "ok" | "warn" | "err" | "info" | "muted" | "accent";

export function WorkersPage() {
  const workers = useWorkers();
  const cores = useCores();
  const [selected, setSelected] = useState<WorkerStatus | null>(null);
  const list = workers.data?.workers ?? [];
  const running = list.filter((worker) => worker.state === "running").length;
  const labels = coreLabelMap(cores.data);

  return (
    <div className="space-y-12 max-w-[1120px]">
      <section className="grid lg:grid-cols-12 gap-10 items-end pt-2">
        <div className="lg:col-span-8 space-y-5">
          <p style={eyebrowStyle}>执行核心 · Workers</p>
          <h1 className="text-[28px] md:text-[38px] leading-[1.15] font-medium" style={{ ...serif, color: "var(--fg-strong)", letterSpacing: 0 }}>
            {running} 个执行核心正在承载流量。
          </h1>
          <p className="max-w-xl text-[15px] leading-[1.9]" style={{ ...sans, color: "var(--fg)" }}>
            Worker 是执行器，由节点的 Core 选择自动决定；生命周期由事务、监督与 Rescue 统一管理。
          </p>
          <div className="flex items-center flex-wrap gap-3">
            <Badge tone={running === list.length && list.length > 0 ? "ok" : "muted"}>
              运行 {running} / {list.length}
            </Badge>
            {workers.isFetching ? <Badge tone="info">刷新中</Badge> : null}
            {workers.error ? <Badge tone="err"><ErrorText error={workers.error} fallback="Worker 状态加载失败" /></Badge> : null}
          </div>
        </div>
        <aside className="lg:col-span-4">
          <Card>
            <div className="px-7 py-7 space-y-3">
              <p style={eyebrowStyle}>调度边界</p>
              <p className="text-[15px] leading-[1.8]" style={{ ...serif, color: "var(--fg-strong)" }}>
                无 Pool 绑定的 Worker 保持停止；单个核心故障不会改变其他核心的 Binding。
              </p>
            </div>
          </Card>
        </aside>
      </section>

      <Rule />

      <section className="space-y-6">
        <SectionHeader eyebrow="运行时" title="Worker 状态" desc="状态每 10 秒从后端刷新。" />
        {workers.isLoading ? (
          <Card><div className="px-7 py-12 text-center text-sm" style={{ color: "var(--muted)" }}>载入中</div></Card>
        ) : workers.error && list.length === 0 ? (
          <Card><div className="px-7 py-8 text-sm" style={{ color: "var(--danger)" }}><ErrorText error={workers.error} fallback="Worker 状态加载失败" /></div></Card>
        ) : list.length === 0 ? (
          <Card><div className="px-7 py-12 text-center text-sm" style={{ color: "var(--muted)" }}>暂无 Worker 状态</div></Card>
        ) : (
          <div className="grid xl:grid-cols-2 gap-6">
            {list.map((worker) => <WorkerCard key={worker.id} worker={worker} labels={labels} onEvents={() => setSelected(worker)} />)}
          </div>
        )}
      </section>

      <WorkerEventsModal worker={selected} labels={labels} onClose={() => setSelected(null)} />
      <Eyebrow>SmartGate · 执行核心</Eyebrow>
    </div>
  );
}

function WorkerCard({ worker, labels, onEvents }: { worker: WorkerStatus; labels: CoreLabelMap; onEvents: () => void }) {
  const tone = stateTone(worker.state);
  return (
    <Card>
      <div className="px-6 py-6 space-y-6">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0">
            <p style={eyebrowStyle}>{worker.id}</p>
            <h2 className="mt-2 text-[22px] font-medium" style={{ ...serif, color: "var(--fg-strong)", letterSpacing: 0 }}>
              {coreLabel(worker.core, labels)}
            </h2>
            <p className="mt-1 text-xs" style={{ ...sans, color: "var(--muted)" }}>
              版本 {worker.version || "未报告"}
            </p>
          </div>
          <Badge tone={tone}><Dot tone={tone} />{stateLabel(worker.state)}</Badge>
        </div>

        <div className="grid grid-cols-3 gap-px" style={{ background: "var(--border)" }}>
          <Metric label="RSS" value={formatRSS(worker.stats?.rss_kb)} />
          <Metric label="FD" value={metricValue(worker.stats?.fd)} />
          <Metric label="连接" value={metricValue(worker.stats?.connections)} />
        </div>

        <div className="space-y-3">
          <p style={eyebrowStyle}>承载 Pool</p>
          <div className="flex flex-wrap gap-2">
            {worker.pools.length > 0 ? worker.pools.map((pool) => <Badge key={pool} tone="accent">{pool}</Badge>) : <span className="text-sm" style={{ color: "var(--muted)" }}>未绑定</span>}
          </div>
        </div>

        <div className="space-y-3">
          <div className="flex items-center justify-between gap-4">
            <p style={eyebrowStyle}>Node Executor · {worker.executors.length}</p>
            <Button variant="ghost" size="sm" onClick={onEvents}>事件日志</Button>
          </div>
          {worker.executors.length === 0 ? (
            <p className="text-sm" style={{ color: "var(--muted)" }}>当前没有执行器</p>
          ) : (
            <div className="divide-y" style={{ borderColor: "var(--border)" }}>
              {worker.executors.map((executor) => (
                <div key={executor.id} className="py-3 flex items-start justify-between gap-4 text-xs">
                  <span className="break-all" style={{ fontFamily: "var(--font-mono)", color: "var(--fg-strong)" }}>{executor.id}</span>
                  <span className="text-right break-all" style={{ ...sans, color: "var(--muted)" }}>{executor.smart_node} · r{executor.revision}</span>
                </div>
              ))}
            </div>
          )}
        </div>

        <div className="pt-4" style={{ borderTop: "1px solid var(--border)" }}>
          <p style={eyebrowStyle}>最近事件</p>
          {worker.last_event ? (
            <div className="mt-2 flex items-start justify-between gap-4 text-xs">
              <span className="min-w-0" style={{ ...sans, color: "var(--fg)" }}>
                <span className="block">{eventMainText(worker.last_event)}</span>
                {eventDiagnostic(worker.last_event) ? (
                  <span className="block mt-1 break-words" style={{ color: "var(--muted)" }}>
                    诊断：{eventDiagnostic(worker.last_event)}
                  </span>
                ) : null}
              </span>
              <time className="shrink-0" style={{ color: "var(--muted)" }}>{formatTime(worker.last_event.at)}</time>
            </div>
          ) : <p className="mt-2 text-xs" style={{ color: "var(--muted)" }}>暂无事件</p>}
        </div>
      </div>
    </Card>
  );
}

function WorkerEventsModal({ worker, labels, onClose }: { worker: WorkerStatus | null; labels: CoreLabelMap; onClose: () => void }) {
  const events = useWorkerEvents(worker?.id ?? null);
  return (
    <Modal
      open={Boolean(worker)}
      onClose={onClose}
      eyebrow={worker?.id}
      title={`${worker ? coreLabel(worker.core, labels) : "Worker"} 事件`}
      width={760}
      footer={<Button variant="ghost" onClick={onClose}>关闭</Button>}
    >
      {events.isLoading ? <p className="text-sm" style={{ color: "var(--muted)" }}>载入中</p> : null}
      {events.error ? <p className="text-sm" style={{ color: "var(--danger)" }}><ErrorText error={events.error} fallback="Worker 事件加载失败" /></p> : null}
      {events.data && events.data.events.length === 0 ? <p className="text-sm" style={{ color: "var(--muted)" }}>暂无事件</p> : null}
      {events.data && events.data.events.length > 0 ? (
        <div className="divide-y" style={{ borderColor: "var(--border)" }}>
          {[...events.data.events].reverse().map((event, index) => <EventRow key={`${event.at}-${index}`} event={event} />)}
        </div>
      ) : null}
    </Modal>
  );
}

function EventRow({ event }: { event: WorkerEvent }) {
  return (
    <div className="py-4 grid sm:grid-cols-[150px_110px_1fr] gap-2 sm:gap-4 text-xs">
      <time style={{ color: "var(--muted)" }}>{formatTime(event.at)}</time>
      <span title={event.kind}><Badge tone={stateTone(event.state)}>{workerEventKindLabel(event.kind)}</Badge></span>
      <span className="break-words" style={{ ...sans, color: "var(--fg)" }}>
        <span className="block">{eventMainText(event)}</span>
        {eventDiagnostic(event) ? (
          <span className="block mt-1" style={{ color: "var(--muted)" }}>诊断：{eventDiagnostic(event)}</span>
        ) : null}
      </span>
    </div>
  );
}

/* 后端 message 若已是中文则作为主文案；英文 message 与 diagnostic 一并归入诊断副行 */
function eventMainText(event: Pick<WorkerEvent, "kind" | "message">): string {
  const message = event.message?.trim();
  if (message && /[㐀-鿿]/u.test(message)) return message;
  return workerEventKindLabel(event.kind);
}

function eventDiagnostic(event: Pick<WorkerEvent, "message" | "diagnostic">): string | undefined {
  const message = event.message?.trim();
  const parts: string[] = [];
  if (message && !/[㐀-鿿]/u.test(message)) parts.push(message);
  if (event.diagnostic && event.diagnostic !== message) parts.push(event.diagnostic);
  return parts.length > 0 ? parts.join("；") : undefined;
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="px-4 py-4 text-center" style={{ background: "var(--surface-2)" }}>
      <p style={eyebrowStyle}>{label}</p>
      <p className="mt-1 text-[18px]" style={{ ...sans, color: "var(--fg-strong)" }}>{value}</p>
    </div>
  );
}

function stateTone(state: WorkerState | string): BadgeTone {
  switch (state) {
    case "running": return "ok";
    case "starting": return "info";
    case "draining": return "warn";
    case "failed": return "err";
    default: return "muted";
  }
}

function stateLabel(state: WorkerState): string {
  return ({ running: "运行中", starting: "启动中", draining: "排空中", failed: "故障", stopped: "已停止" })[state];
}

function metricValue(value?: number): string {
  return value && value > 0 ? String(value) : "—";
}

function formatRSS(value?: number): string {
  if (!value || value <= 0) return "—";
  return value >= 1024 ? `${(value / 1024).toFixed(1)} MiB` : `${value} KiB`;
}

function formatTime(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString();
}
