import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useOverview, useConfigState, useRuntimeStatus, useConnections } from "../lib/queries";
import { openEvents } from "../lib/sse";
import type { StatusEvent } from "../lib/sse";
import type { ConnectionsData, Overview as OverviewData, RuntimeStatus } from "../lib/types";
import { Badge, Dot } from "../ui/Badge";
import { Card, Eyebrow, SectionHeader, Rule } from "../ui/Card";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { api } from "../lib/api";
import type { VersionInfo } from "../lib/types";
import { gatewayModeHeadline, gatewayModeSummary } from "./gatewayState";
import { ErrorText } from "../ui/ErrorText";
import { runtimePhaseLabel, rescueReasonLabel } from "../lib/statusText";

export function OverviewPage() {
  const overview = useOverview();
  const configState = useConfigState();
  const runtime = useRuntimeStatus();
  // 概览只要连接摘要，低频轮询即可
  const connections = useConnections(15_000);
  const [liveStatus, setLiveStatus] = useState<StatusEvent | null>(null);
  const [sseAlive, setSseAlive] = useState<"connecting" | "live" | "down">("connecting");
  const [version, setVersion] = useState<VersionInfo | null>(null);

  useEffect(() => {
    const close = openEvents({
      onOpen: () => setSseAlive("live"),
      onStatus: (ev) => {
        setLiveStatus(ev);
        setSseAlive("live");
      },
      onError: () => setSseAlive("down"),
    });
    return close;
  }, []);

  useEffect(() => {
    let cancelled = false;
    api
      .get<VersionInfo>("/version")
      .then((v) => {
        if (!cancelled) setVersion(v);
      })
      .catch(() => {
        /* 版本接口非关键，忽略 */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const data = overview.data;
  const cfg = configState.data || data?.config;
  const rt = (runtime.data as RuntimeStatus | undefined) ||
    (liveStatus?.runtime as RuntimeStatus | undefined) ||
    (data?.runtime as RuntimeStatus | undefined);

  return (
    <div className="space-y-14 max-w-[1120px]">
      <Hero
        data={data}
        rt={rt}
        sseAlive={sseAlive}
        loading={overview.isLoading}
        error={overview.error}
      />

      <Rule />

      <section className="space-y-7">
        <SectionHeader
          eyebrow="数据概览"
          title="当前状态"
          desc="数据每 15 秒自动刷新，运行阶段通过 SSE 实时推送。"
          right={<SseBadge state={sseAlive} />}
        />
        <MetricsGrid data={data} cfg={cfg} connections={connections.data} />
      </section>

      <section className="grid lg:grid-cols-12 gap-10">
        <div className="lg:col-span-7 space-y-6">
          <SectionHeader eyebrow="运行时" title="Runtime 详情" />
          <RuntimeCard rt={rt} />
        </div>
        <div className="lg:col-span-5 space-y-6">
          <SectionHeader eyebrow="后端" title="版本信息" />
          <VersionCard version={version} />
        </div>
      </section>

      <Footer />
    </div>
  );
}

function Hero({
  data,
  rt,
  sseAlive,
  loading,
  error,
}: {
  data: OverviewData | undefined;
  rt: RuntimeStatus | undefined;
  sseAlive: "connecting" | "live" | "down";
  loading: boolean;
  error: unknown;
}) {
  const savedMode = data?.gateway?.saved_mode;
  const appliedMode = data?.gateway?.applied_mode;
  const runningMode = data?.gateway?.running_mode ?? data?.gateway?.mode;
  const dirty = data?.config?.dirty;
  const phase = rt?.phase ?? "—";

  return (
    <section className="grid lg:grid-cols-12 gap-10 items-end pt-2">
      <div className="lg:col-span-8 space-y-5">
        <p style={eyebrowStyle}>概览 · Overview</p>
        <h1
          className="text-[28px] md:text-[38px] leading-[1.15] font-medium"
          style={{ ...sans, color: "var(--fg-strong)", letterSpacing: "-0.015em" }}
        >
          {gatewayModeHeadline(runningMode)}
        </h1>
        <p className="max-w-xl text-[16px] leading-[1.9]" style={{ ...sans, color: "var(--fg)" }}>
          {savedMode
            ? gatewayModeSummary({ saved: savedMode, applied: appliedMode, running: runningMode, dirty: Boolean(dirty) })
            : "网关状态等待回话。"}
        </p>
        <div className="flex items-center flex-wrap gap-3 text-sm" style={sans}>
          <span title={phase}>
            <Badge tone={overviewPhaseTone(phase)}>
              <Dot tone={overviewPhaseTone(phase)} />
              运行阶段 · {phase === "—" ? "—" : runtimePhaseLabel(phase)}
            </Badge>
          </span>
          <Badge tone={sseAlive === "live" ? "ok" : sseAlive === "down" ? "err" : "muted"}>
            事件流 · {sseAlive === "live" ? "已连接" : sseAlive === "down" ? "已断开" : "连接中"}
          </Badge>
          {rt?.rescue_active ? <Badge tone="warn">救援模式</Badge> : null}
          {loading ? <Badge tone="muted">载入中…</Badge> : null}
          {error ? <Badge tone="err"><ErrorText error={error} fallback="概览加载失败" /></Badge> : null}
        </div>
      </div>

      <aside className="lg:col-span-4">
        <Card>
          <div className="px-7 py-7 space-y-3">
            <p style={eyebrowStyle}>提示</p>
            <p className="text-[15px] leading-[1.85]" style={{ ...serif, color: "var(--fg-strong)" }}>
              修改"期望配置"后，必须演练并应用，配置才会真正生效。
            </p>
            <p className="text-xs" style={{ ...sans, color: "var(--muted)" }}>
              dry-run → apply 是 SmartGate 的安全门。
            </p>
          </div>
        </Card>
      </aside>
    </section>
  );
}

function overviewPhaseTone(phase: string): "ok" | "warn" | "err" | "info" | "muted" {
  if (phase === "running") return "ok";
  if (phase === "rescue") return "warn";
  if (phase === "stopped") return "err";
  if (phase === "starting") return "info";
  return "muted";
}

function MetricsGrid({
  data,
  cfg,
  connections,
}: {
  data: OverviewData | undefined;
  cfg: { dirty: boolean; config_epoch: number; gateway_mode: string } | undefined;
  connections: ConnectionsData | undefined;
}) {
  const staleConnections = connections
    ? connections.connections.filter((c) => c.stale_generation).length
    : 0;
  const items: { label: string; value: string; hint: string; tone?: "warn"; to?: string }[] = [
    {
      label: "活跃节点",
      value: data ? `${data.nodes.usable} / ${data.nodes.total}` : "—",
      hint: data?.pending?.dirty
        ? `运行中；待应用 ${data.pending.nodes.usable} / ${data.pending.nodes.total}`
        : data
          ? "运行中可用 / 总数"
          : "等待数据",
    },
    {
      label: "订阅源",
      value: data ? `${data.providers.enabled} / ${data.providers.total}` : "—",
      hint: data?.pending?.dirty
        ? `运行中；待应用 ${data.pending.providers.enabled} / ${data.pending.providers.total}`
        : data
          ? "运行中启用 / 注册"
          : "等待数据",
    },
    {
      label: "业务池",
      value: data ? `${data.business_pools.total}` : "—",
      hint: data?.pending?.dirty ? `运行中；待应用 ${data.pending.business_pools.total}` : "运行中 Pools",
    },
    {
      label: "活跃连接",
      value: connections ? `${connections.connections.length}` : "—",
      hint: connections
        ? `下行 ${formatTraffic(connections.download_total)} · 上行 ${formatTraffic(connections.upload_total)}${staleConnections > 0 ? ` · 排空中 ${staleConnections}` : ""}`
        : "等待数据",
      to: "/connections",
    },
    {
      label: "config_epoch",
      value: cfg ? `${cfg.config_epoch}` : "—",
      hint: cfg?.dirty ? "存在未应用变更" : "已应用",
      tone: cfg?.dirty ? ("warn" as const) : undefined,
    },
  ];

  return (
    <div className="grid grid-cols-2 lg:grid-cols-5 gap-px" style={{ background: "var(--border)" }}>
      {items.map((m) => {
        const cell = (
          <div className="px-6 py-7 h-full" style={{ background: "var(--surface)" }}>
            <p style={eyebrowStyle}>{m.label}</p>
            <p
              className="text-[40px] mt-2 font-medium"
              style={{
                ...serif,
                color: "var(--fg-strong)",
                letterSpacing: "-0.025em",
                lineHeight: 1.1,
              }}
            >
              {m.value}
            </p>
            <p
              className="mt-3 text-xs"
              style={{ ...sans, color: m.tone === "warn" ? "var(--warning)" : "var(--muted)" }}
            >
              {m.hint}
            </p>
          </div>
        );
        return m.to ? (
          <Link
            key={m.label}
            to={m.to}
            className="block transition-opacity hover:opacity-80"
            title="查看连接页"
          >
            {cell}
          </Link>
        ) : (
          <div key={m.label}>{cell}</div>
        );
      })}
    </div>
  );
}

function formatTraffic(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let normalized = value;
  let index = 0;
  while (normalized >= 1024 && index < units.length - 1) {
    normalized /= 1024;
    index++;
  }
  return `${normalized.toFixed(normalized >= 10 || index === 0 ? 0 : 1)} ${units[index]}`;
}

function RuntimeCard({ rt }: { rt: RuntimeStatus | undefined }) {
  return (
    <Card>
      <dl className="divide-y" style={{ borderColor: "var(--border)" }}>
        <KV label="运行阶段" value={rt?.phase ? runtimePhaseLabel(rt.phase) : "—"} />
        <KV label="监听地址" value={rt?.listen_addr || "—"} mono />
        <KV label="config_epoch" value={rt?.config_epoch?.toString() || "—"} />
        <KV
          label="topology_hash"
          value={rt?.topology_hash ? `${rt.topology_hash.slice(0, 18)}…` : "—"}
          mono
        />
        <KV
          label="救援模式"
          value={
            rt?.rescue_active ? (
              <span className="space-y-1 block">
                <Badge tone="warn">已进入 · <span title={rt.rescue_reason || "manual"}>{rescueReasonLabel(rt.rescue_reason)}</span></Badge>
                <span className="block text-xs" style={{ color: "var(--muted)" }}>代理已全部停止、全屋直连；恢复代理请清除救援状态</span>
              </span>
            ) : (
              <Badge tone="ok">未启用</Badge>
            )
          }
        />
      </dl>
    </Card>
  );
}

function VersionCard({ version }: { version: VersionInfo | null }) {
  return (
    <Card>
      <dl className="divide-y" style={{ borderColor: "var(--border)" }}>
        <KV label="守护进程" value={version?.name || "—"} />
        <KV label="版本" value={version?.version || "—"} mono />
        <KV label="构建时间" value={formatTime(version?.built_at)} />
        <KV
          label="commit"
          value={version?.commit ? version.commit.slice(0, 12) : "—"}
          mono
        />
        <KV
          label="平台"
          value={version ? `${version.goos}/${version.goarch} · ${version.go_version}` : "—"}
          mono
        />
        <KV label="构建时间" value={version?.built_at || "—"} mono />
      </dl>
    </Card>
  );
}

function KV({
  label,
  value,
  mono,
}: {
  label: string;
  value: React.ReactNode;
  mono?: boolean;
}) {
  return (
    <div className="flex items-baseline justify-between gap-6 px-7 py-4">
      <span className="text-xs" style={{ ...sans, color: "var(--muted)" }}>
        {label}
      </span>
      <span
        className="text-sm text-right break-all"
        style={{
          color: "var(--fg-strong)",
          fontFamily: mono ? "var(--font-mono)" : "var(--font-sans)",
        }}
      >
        {value}
      </span>
    </div>
  );
}

function formatTime(s: string | undefined | null): string {
  if (!s) return "—";
  const d = new Date(s);
  if (isNaN(d.getTime())) return "—";
  return d.toLocaleString();
}

function SseBadge({ state }: { state: "connecting" | "live" | "down" }) {
  if (state === "live") return <Badge tone="ok"><Dot tone="ok" />SSE 已连接</Badge>;
  if (state === "down") return <Badge tone="err">SSE 已断开</Badge>;
  return <Badge tone="muted">SSE 连接中…</Badge>;
}

function Footer() {
  return (
    <Eyebrow>
      <span style={{ letterSpacing: "0.3em" }}>SmartGate · 书房 · 第一期 · 概览与网关</span>
    </Eyebrow>
  );
}
