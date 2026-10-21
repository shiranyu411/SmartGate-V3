import { useEffect, useMemo, useRef, useState } from "react";
import {
  useBusinessPools,
  useCloseConnections,
  useConnections,
  useCores,
  useRuntimeStatus,
  useSmartNodes,
} from "../lib/queries";
import type {
  ConnectionCloseResult,
  ConnectionItem,
  CoreID,
  DrainProgress,
  PoolBinding,
} from "../lib/types";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Card, Eyebrow, Rule, SectionHeader } from "../ui/Card";
import { Field, Input, Select } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { Segmented } from "../ui/Segmented";
import { eyebrowStyle, mono, sans, serif } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { drainStatusLabel } from "../lib/statusText";
import { coreLabel, coreLabelMap, type CoreLabelMap } from "../lib/coreLabels";

type Interval = "1s" | "2s" | "5s" | "10s" | "off";
type CloseTarget = { scope: "binding"; pool: string } | { scope: "all" } | null;
type RateSample = { d: number; u: number };

const intervalMs: Record<Interval, number> = {
  "1s": 1_000,
  "2s": 2_000,
  "5s": 5_000,
  "10s": 10_000,
  off: 0,
};

const emptyConnections: ConnectionItem[] = [];
const HISTORY_LIMIT = 60;

export function ConnectionsPage() {
  const [interval, setInterval] = useState<Interval>("5s");
  const [filter, setFilter] = useState("");
  const [poolFilter, setPoolFilter] = useState("all");
  const [routeFilter, setRouteFilter] = useState("all");
  const [workerFilter, setWorkerFilter] = useState("all");
  const [closeTarget, setCloseTarget] = useState<CloseTarget>(null);
  const [closingID, setClosingID] = useState("");
  const [pageError, setPageError] = useState<unknown>(null);
  const [closeResult, setCloseResult] = useState<ConnectionCloseResult | null>(null);
  const connections = useConnections(intervalMs[interval]);
  const runtime = useRuntimeStatus();
  const businessPools = useBusinessPools();
  const smartNodes = useSmartNodes();
  const cores = useCores();
  const close = useCloseConnections();

  /* —— 中文名映射：池名来自 BusinessPool.name，节点名来自 SmartNode.display_name —— */
  const poolNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const pool of businessPools.data?.business_pools ?? []) {
      map.set(pool.id, pool.name || pool.id);
    }
    return map;
  }, [businessPools.data]);
  const nodeNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const node of smartNodes.data?.nodes ?? []) {
      map.set(node.id, node.display_name || node.id);
    }
    return map;
  }, [smartNodes.data]);
  const coreLabels = useMemo(() => coreLabelMap(cores.data), [cores.data]);
  const poolLabel = (id: string) => (id ? poolNameMap.get(id) ?? id : "未归属");
  const nodeLabel = (id: string) => (id ? nodeNameMap.get(id) ?? id : "");
  const chainLabel = (entry: string) => {
    if (entry === "DIRECT") return "直连";
    if (entry.toLowerCase() === "proxy") return "代理";
    return poolNameMap.get(entry) ?? nodeNameMap.get(entry) ?? entry;
  };

  /* —— 速率历史：轮询间对累计流量做差，前端积累最近 60 个采样 —— */
  const [history, setHistory] = useState<RateSample[]>([]);
  const [connRates, setConnRates] = useState<Map<string, RateSample>>(new Map());
  const prevSampleRef = useRef<{
    down: number;
    up: number;
    at: number;
    perConn: Map<string, RateSample>;
  } | null>(null);

  useEffect(() => {
    const data = connections.data;
    if (!data) return;
    const at = connections.dataUpdatedAt;
    const perConnNow = new Map<string, RateSample>(
      data.connections.map((c) => [c.id, { d: c.download, u: c.upload }]),
    );
    const prev = prevSampleRef.current;
    if (prev && at > prev.at) {
      const dt = (at - prev.at) / 1000;
      if (dt > 0.2) {
        const d = Math.max(0, (data.download_total - prev.down) / dt);
        const u = Math.max(0, (data.upload_total - prev.up) / dt);
        const rates = new Map<string, RateSample>();
        for (const c of data.connections) {
          const p = prev.perConn.get(c.id);
          if (p) {
            rates.set(c.id, {
              d: Math.max(0, (c.download - p.d) / dt),
              u: Math.max(0, (c.upload - p.u) / dt),
            });
          }
        }
        setHistory((h) => [...h.slice(-(HISTORY_LIMIT - 1)), { d, u }]);
        setConnRates(rates);
      }
    }
    prevSampleRef.current = { down: data.download_total, up: data.upload_total, at, perConn: perConnNow };
  }, [connections.data, connections.dataUpdatedAt]);

  const list = connections.data?.connections ?? emptyConnections;
  const pools = useMemo(
    () => [...new Set(list.map((connection) => connection.pool).filter(Boolean))].sort(),
    [list],
  );
  const workers = useMemo(
    () => [...new Set(list.map((connection) => connection.worker).filter(Boolean))].sort(),
    [list],
  );

  useEffect(() => {
    if (poolFilter !== "all" && !pools.includes(poolFilter)) setPoolFilter("all");
  }, [poolFilter, pools]);

  useEffect(() => {
    if (workerFilter !== "all" && !workers.includes(workerFilter)) setWorkerFilter("all");
  }, [workerFilter, workers]);

  const filtered = useMemo(() => {
    const query = filter.trim().toLowerCase();
    return list.filter((connection) => {
      if (poolFilter !== "all" && connection.pool !== poolFilter) return false;
      if (routeFilter === "current" && connection.stale_generation) return false;
      if (routeFilter === "previous" && !connection.stale_generation) return false;
      if (workerFilter !== "all" && connection.worker !== workerFilter) return false;
      if (!query) return true;
      const metadata = connection.metadata ?? {};
      return [
        connection.id,
        connection.pool,
        poolLabel(connection.pool),
        connection.worker,
        connection.smart_node,
        nodeLabel(connection.smart_node),
        connection.core,
        metadata.host,
        ...(Array.isArray(metadata.dnsHosts) ? metadata.dnsHosts : []),
        metadata.sourceIP,
        metadata.destinationIP,
        ...connection.chains,
      ]
        .filter((value) => value !== undefined && value !== null && value !== "")
        .join(" ")
        .toLowerCase()
        .includes(query);
    });
    // poolLabel/nodeLabel 只依赖下面两个 map
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter, routeFilter, list, poolFilter, workerFilter, poolNameMap, nodeNameMap]);

  async function closeSingle(connection: ConnectionItem) {
    setClosingID(connection.id);
    setPageError(null);
    setCloseResult(null);
    try {
      const result = await close.mutateAsync({ scope: "single", id: connection.id });
      setCloseResult(result);
    } catch (error) {
      setPageError(error);
    } finally {
      setClosingID("");
    }
  }

  async function confirmClose() {
    if (!closeTarget) return;
    setPageError(null);
    setCloseResult(null);
    try {
      const result = closeTarget.scope === "all"
        ? await close.mutateAsync({ scope: "all", confirm: true })
        : await close.mutateAsync({ scope: "binding", pool: closeTarget.pool, confirm: true });
      setCloseResult(result);
      setCloseTarget(null);
    } catch {
      // 弹窗内就地展示 mutation 错误
    }
  }

  const totalDown = connections.data?.download_total ?? 0;
  const totalUp = connections.data?.upload_total ?? 0;
  const memory = connections.data?.memory ?? 0;
  const staleCount = useMemo(() => list.filter((c) => c.stale_generation).length, [list]);
  const latest = history.length > 0 ? history[history.length - 1] : undefined;

  return (
    <div className="space-y-12 max-w-[1440px]">
      <Hero
        loading={connections.isLoading}
        error={connections.error}
        count={filtered.length}
        totalCount={list.length}
        totalDown={totalDown}
        totalUp={totalUp}
        memory={memory}
        history={history}
        intervalKey={interval}
      />

      <Rule />

      <StatStrip
        count={list.length}
        staleCount={staleCount}
        latest={latest}
        totalDown={totalDown}
        totalUp={totalUp}
        memory={memory}
        history={history}
      />

      <Rule />

      <section className="grid grid-cols-1 xl:grid-cols-12 gap-6 items-stretch">
        <div className="xl:col-span-5">
          <BindingPanel
            bindings={runtime.data?.bindings ?? []}
            drains={runtime.data?.drains ?? []}
            poolLabel={poolLabel}
            nodeLabel={nodeLabel}
          />
        </div>
        <div className="xl:col-span-3">
          <PoolDistribution
            connections={list}
            activePool={poolFilter}
            poolLabel={poolLabel}
            onSelect={(pool) => setPoolFilter((current) => (current === pool ? "all" : pool))}
          />
        </div>
        <div className="xl:col-span-4">
          <TopHosts connections={list} />
        </div>
      </section>

      <Rule />

      <section className="space-y-6">
        <SectionHeader
          eyebrow="实时连接"
          title="活动会话"
          desc="旧线路连接会标记为「排空中」；按行关闭只影响单条连接，按业务池关闭会同时清理当前线路与排空中的旧线路。"
          right={
            <div className="flex items-center gap-2 flex-wrap">
              <Segmented
                value={interval}
                onChange={setInterval}
                options={[
                  { key: "1s", label: "1s" },
                  { key: "2s", label: "2s" },
                  { key: "5s", label: "5s" },
                  { key: "10s", label: "10s" },
                  { key: "off", label: "暂停" },
                ]}
              />
              <Button
                size="sm"
                variant="danger"
                disabled={poolFilter === "all"}
                onClick={() => poolFilter !== "all" && setCloseTarget({ scope: "binding", pool: poolFilter })}
              >
                {poolFilter === "all" ? "关闭所选业务池" : `关闭「${poolLabel(poolFilter)}」`}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setCloseTarget({ scope: "all" })}>
                全局关闭（诊断）
              </Button>
            </div>
          }
        />

        <div className="grid sm:grid-cols-2 lg:grid-cols-4 gap-3">
          <Input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder="搜索 域名 / IP / 节点 / 链路"
            aria-label="搜索连接"
          />
          <Select value={poolFilter} onChange={(event) => setPoolFilter(event.target.value)} aria-label="按业务池筛选">
            <option value="all">全部业务池</option>
            {pools.map((pool) => <option key={pool} value={pool}>{poolLabel(pool)}</option>)}
          </Select>
          <Select
            value={routeFilter}
            onChange={(event) => setRouteFilter(event.target.value)}
            aria-label="按线路状态筛选"
          >
            <option value="all">全部线路状态</option>
            <option value="current">当前线路</option>
            <option value="previous">切换前的旧线路</option>
          </Select>
          <Select value={workerFilter} onChange={(event) => setWorkerFilter(event.target.value)} aria-label="按执行核心筛选">
            <option value="all">全部执行核心</option>
            {workers.map((worker) => <option key={worker} value={worker}>{worker}</option>)}
          </Select>
        </div>

        <p className="text-xs leading-relaxed -mt-1" style={{ ...sans, color: "var(--muted)" }}>
          目标优先显示核心上报的域名，其次显示近期 DNS 关联；未观察到域名时保留 IP。线路切换后，旧连接会标记为旧线路。
        </p>

        {closeResult ? <CloseResultNotice result={closeResult} poolLabel={poolLabel} /> : null}
        {pageError ? (
          <Card>
            <div className="px-7 py-5 text-sm break-words" style={{ ...sans, color: "var(--danger)" }}>
              <ErrorText error={pageError} fallback="关闭连接失败" showDiagnostic />
            </div>
          </Card>
        ) : null}

        {!connections.data && connections.error ? (
          <Card>
            <div className="px-7 py-7 text-sm" style={{ ...sans, color: "var(--danger)" }}>
              <ErrorText error={connections.error} fallback="连接列表加载失败" showDiagnostic />
            </div>
          </Card>
        ) : filtered.length === 0 ? (
          <Card>
            <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
              {connections.isLoading ? "载入中…" : "暂无匹配连接"}
            </div>
          </Card>
        ) : (
          <ConnectionsTable
            connections={filtered}
            closingID={closingID}
            connRates={connRates}
            poolLabel={poolLabel}
            nodeLabel={nodeLabel}
            chainLabel={chainLabel}
            coreLabels={coreLabels}
            onClose={(connection) => void closeSingle(connection)}
          />
        )}
      </section>

      <ConfirmCloseModal
        target={closeTarget}
        poolLabel={poolLabel}
        loading={close.isPending}
        error={close.error}
        onClose={() => {
          close.reset();
          setCloseTarget(null);
        }}
        onConfirm={() => void confirmClose()}
      />

      <Eyebrow>SmartGate · 书房 · 第三期 · 连接</Eyebrow>
    </div>
  );
}

/* ═══════════ Hero：左文右图 ═══════════ */

function Hero({
  loading,
  error,
  count,
  totalCount,
  totalDown,
  totalUp,
  memory,
  history,
  intervalKey,
}: {
  loading: boolean;
  error: unknown;
  count: number;
  totalCount: number;
  totalDown: number;
  totalUp: number;
  memory: number;
  history: RateSample[];
  intervalKey: Interval;
}) {
  return (
    <section className="grid lg:grid-cols-12 gap-10 items-stretch pt-2">
      <div className="lg:col-span-7 flex flex-col justify-center gap-5">
        <p style={eyebrowStyle}>连接 · Connections</p>
        <h1
          className="text-[28px] md:text-[38px] leading-[1.15] font-medium"
          style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.025em" }}
        >
          房间里现在<br />
          显示 {count} / {totalCount} 通对话。
        </h1>
        <p className="max-w-2xl text-[15px] leading-[1.9]" style={{ ...sans, color: "var(--fg)" }}>
          SmartGate 汇总各执行核心的实时会话，并标注每个业务池当前生效的出口。平滑切换期间，旧线路连接保持可见，直到排空完成。
        </p>
        <div className="flex items-center flex-wrap gap-3 text-sm" style={sans}>
          <Badge tone="info">下行累计 {formatBytes(totalDown)}</Badge>
          <Badge tone="accent">上行累计 {formatBytes(totalUp)}</Badge>
          <Badge tone="muted">核心内存 {formatBytes(memory)}</Badge>
          {loading ? <Badge tone="muted">载入中…</Badge> : null}
          {error ? <Badge tone="err"><ErrorText error={error} fallback="连接列表加载失败" /></Badge> : null}
        </div>
      </div>
      <aside className="lg:col-span-5">
        <TrafficChartCard history={history} intervalKey={intervalKey} />
      </aside>
    </section>
  );
}

/* ═══════════ 流量趋势图 ═══════════ */

const CHART_W = 420;
const CHART_PAD = { left: 8, right: 60, top: 14, bottom: 10 };

function TrafficChartCard({ history, intervalKey }: { history: RateSample[]; intervalKey: Interval }) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const [chartH, setChartH] = useState(150);
  const [hoverX, setHoverX] = useState<number | null>(null);

  useEffect(() => {
    const el = boxRef.current;
    if (!el) return;
    const observer = new ResizeObserver(() => {
      const rect = el.getBoundingClientRect();
      if (rect.width > 40 && rect.height > 20) {
        setChartH(Math.max(110, Math.round((CHART_W * rect.height) / rect.width)));
      }
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const secondsPerSample = intervalMs[intervalKey] > 0 ? intervalMs[intervalKey] / 1000 : 5;

  return (
    <Card className="h-full" style={{ display: "flex", flexDirection: "column", padding: "24px 28px 20px" }}>
      <div className="flex items-baseline justify-between gap-2 mb-4">
        <div>
          <p style={eyebrowStyle}>实时流量</p>
          <p className="text-[17px] mt-1 font-medium" style={{ ...serif, color: "var(--fg-strong)" }}>
            最近 {HISTORY_LIMIT} 次采样吞吐
          </p>
        </div>
        <Badge tone="muted">{intervalKey === "off" ? "已暂停" : `采样 ${intervalKey}`}</Badge>
      </div>
      <div
        ref={boxRef}
        className="relative flex-1"
        style={{ minHeight: 130 }}
        onMouseMove={(event) => {
          const rect = event.currentTarget.getBoundingClientRect();
          setHoverX(((event.clientX - rect.left) / rect.width) * CHART_W);
        }}
        onMouseLeave={() => setHoverX(null)}
      >
        <TrafficChartSvg history={history} chartH={chartH} hoverX={hoverX} secondsPerSample={secondsPerSample} />
      </div>
      <div className="flex gap-4 mt-3 text-xs" style={{ ...sans, color: "var(--muted)" }}>
        <span className="inline-flex items-center gap-1.5">
          <span className="size-2 rounded-full inline-block" style={{ background: "var(--info)" }} />下行
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="size-2 rounded-full inline-block" style={{ background: "var(--accent)" }} />上行
        </span>
      </div>
      <p className="text-xs mt-2.5 leading-relaxed" style={{ ...sans, color: "var(--muted)" }}>
        切换不依赖全局断流；日常诊断优先关闭单条连接或单个业务池。
      </p>
    </Card>
  );
}

function TrafficChartSvg({
  history,
  chartH,
  hoverX,
  secondsPerSample,
}: {
  history: RateSample[];
  chartH: number;
  hoverX: number | null;
  secondsPerSample: number;
}) {
  if (history.length < 2) {
    return (
      <div className="absolute inset-0 grid place-items-center text-xs" style={{ ...sans, color: "var(--muted)" }}>
        正在积累速率样本…
      </div>
    );
  }

  const { left, right, top, bottom } = CHART_PAD;
  const plotW = CHART_W - left - right;
  const max = Math.max(...history.map((s) => Math.max(s.d, s.u)), 1) * 1.15;
  const x = (index: number) => left + ((index + (HISTORY_LIMIT - history.length)) / (HISTORY_LIMIT - 1)) * plotW;
  const y = (value: number) => top + (1 - value / max) * (chartH - top - bottom);

  const linePath = (pick: (s: RateSample) => number) =>
    history.map((s, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(pick(s)).toFixed(1)}`).join("");
  const areaPath = (pick: (s: RateSample) => number) =>
    `${linePath(pick)}L${x(history.length - 1).toFixed(1)},${y(0).toFixed(1)}L${x(0).toFixed(1)},${y(0).toFixed(1)}Z`;

  const lastIndex = history.length - 1;
  const lastSample = history[lastIndex];

  let hoverIndex: number | null = null;
  if (hoverX !== null) {
    const fraction = Math.min(1, Math.max(0, (hoverX - left) / plotW));
    const index = Math.round(fraction * (HISTORY_LIMIT - 1)) - (HISTORY_LIMIT - history.length);
    if (index >= 0 && index < history.length) hoverIndex = index;
  }
  const hoverSample = hoverIndex !== null ? history[hoverIndex] : undefined;

  return (
    <>
      <svg
        viewBox={`0 0 ${CHART_W} ${chartH}`}
        preserveAspectRatio="none"
        className="absolute inset-0 w-full h-full"
        aria-label="下行与上行速率折线图"
      >
        {[max / 2, max].map((tick) => (
          <g key={tick}>
            <line
              x1={left}
              y1={y(tick)}
              x2={CHART_W - right}
              y2={y(tick)}
              stroke="var(--border)"
              strokeWidth={1}
              opacity={0.55}
            />
            <text
              x={CHART_W - right + 5}
              y={y(tick) + 3}
              fontSize={9}
              fill="var(--muted)"
              style={sans}
            >
              {formatRate(tick)}
            </text>
          </g>
        ))}
        <line x1={left} y1={y(0)} x2={CHART_W - right} y2={y(0)} stroke="var(--border)" strokeWidth={1} opacity={0.8} />
        <path d={areaPath((s) => s.d)} fill="var(--info)" opacity={0.1} />
        <path d={areaPath((s) => s.u)} fill="var(--accent)" opacity={0.1} />
        <path d={linePath((s) => s.d)} fill="none" stroke="var(--info)" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />
        <path d={linePath((s) => s.u)} fill="none" stroke="var(--accent)" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />
        {lastSample ? (
          <>
            <circle cx={x(lastIndex)} cy={y(lastSample.d)} r={4} fill="var(--info)" stroke="var(--surface)" strokeWidth={2} />
            <circle cx={x(lastIndex)} cy={y(lastSample.u)} r={4} fill="var(--accent)" stroke="var(--surface)" strokeWidth={2} />
          </>
        ) : null}
        {hoverIndex !== null && hoverSample ? (
          <>
            <line x1={x(hoverIndex)} y1={top} x2={x(hoverIndex)} y2={y(0)} stroke="var(--border-strong)" strokeWidth={1} />
            <circle cx={x(hoverIndex)} cy={y(hoverSample.d)} r={3.5} fill="var(--info)" stroke="var(--surface)" strokeWidth={2} />
            <circle cx={x(hoverIndex)} cy={y(hoverSample.u)} r={3.5} fill="var(--accent)" stroke="var(--surface)" strokeWidth={2} />
          </>
        ) : null}
      </svg>
      {hoverIndex !== null && hoverSample ? (
        <div
          className="absolute z-10 pointer-events-none text-[11px] leading-relaxed whitespace-nowrap px-2.5 py-1.5"
          style={{
            ...mono,
            left: `min(calc(${((x(hoverIndex) / CHART_W) * 100).toFixed(1)}% + 12px), calc(100% - 150px))`,
            top: 6,
            background: "var(--surface)",
            border: "1px solid var(--border-strong)",
            borderRadius: "var(--radius-sm)",
            boxShadow: "var(--shadow-float)",
            color: "var(--fg-strong)",
          }}
        >
          <span style={{ ...sans, color: "var(--muted)" }}>
            {Math.round((history.length - 1 - hoverIndex) * secondsPerSample)} 秒前
          </span>
          <br />
          <span className="inline-block size-[7px] rounded-full mr-1.5 align-[1px]" style={{ background: "var(--info)" }} />
          下行 {formatRate(hoverSample.d)}
          <br />
          <span className="inline-block size-[7px] rounded-full mr-1.5 align-[1px]" style={{ background: "var(--accent)" }} />
          上行 {formatRate(hoverSample.u)}
        </div>
      ) : null}
    </>
  );
}

/* ═══════════ 统计带 ═══════════ */

function StatStrip({
  count,
  staleCount,
  latest,
  totalDown,
  totalUp,
  memory,
  history,
}: {
  count: number;
  staleCount: number;
  latest: RateSample | undefined;
  totalDown: number;
  totalUp: number;
  memory: number;
  history: RateSample[];
}) {
  const items = [
    {
      label: "活跃连接",
      value: String(count),
      unit: "",
      sub: staleCount > 0 ? `含排空中 ${staleCount} 条` : "无排空连接",
      spark: undefined as number[] | undefined,
    },
    {
      label: "下行速率",
      ...splitRate(latest?.d),
      sub: `累计 ${formatBytes(totalDown)}`,
      spark: history.map((s) => s.d),
    },
    {
      label: "上行速率",
      ...splitRate(latest?.u),
      sub: `累计 ${formatBytes(totalUp)}`,
      spark: history.map((s) => s.u),
    },
    {
      label: "核心内存",
      ...splitBytes(memory),
      sub: "Worker 观测端常驻开销",
      spark: undefined,
    },
  ];
  return (
    <section className="grid grid-cols-2 lg:grid-cols-4">
      {items.map((item, index) => (
        <div
          key={item.label}
          className={`flex flex-col gap-2 py-1 pr-9 ${index > 0 ? "lg:border-l lg:pl-9" : ""} ${index % 2 === 1 ? "max-lg:border-l max-lg:pl-6" : ""}`}
          style={{ borderColor: "var(--border)" }}
        >
          <p style={eyebrowStyle}>{item.label}</p>
          <p
            className="text-[34px] leading-[1.1] font-medium"
            style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.02em" }}
          >
            {item.value}
            {item.unit ? (
              <span className="text-[17px] font-normal" style={{ color: "var(--muted)", letterSpacing: 0 }}>
                {" "}{item.unit}
              </span>
            ) : null}
          </p>
          <p className="text-[12.5px]" style={{ ...sans, color: "var(--muted)" }}>{item.sub}</p>
          {item.spark && item.spark.length >= 2 ? <Sparkline values={item.spark.slice(-12)} /> : null}
        </div>
      ))}
    </section>
  );
}

function Sparkline({ values }: { values: number[] }) {
  const W = 150;
  const H = 22;
  const max = Math.max(...values, 1);
  const step = W / 11;
  const offset = 12 - values.length;
  const y = (value: number) => H - 3 - (value / max) * (H - 8);
  const path = values
    .map((value, index) => `${index === 0 ? "M" : "L"}${((index + offset) * step).toFixed(1)},${y(value).toFixed(1)}`)
    .join("");
  const last = values[values.length - 1];
  return (
    <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="w-full max-w-[150px] h-[22px] mt-0.5" aria-hidden>
      <path d={path} fill="none" stroke="var(--border-strong)" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round" />
      {last !== undefined ? (
        <circle cx={W} cy={y(last)} r={3} fill="var(--accent)" stroke="var(--surface)" strokeWidth={2} />
      ) : null}
    </svg>
  );
}

/* ═══════════ 出口绑定 / 排空 ═══════════ */

function BindingPanel({
  bindings,
  drains,
  poolLabel,
  nodeLabel,
}: {
  bindings: PoolBinding[];
  drains: DrainProgress[];
  poolLabel: (id: string) => string;
  nodeLabel: (id: string) => string;
}) {
  const drainByPool = useMemo(() => new Map(drains.map((drain) => [drain.pool, drain])), [drains]);
  return (
    <Card className="h-full" style={{ padding: 28 }}>
      <div className="mb-4">
        <p style={eyebrowStyle}>出口 · 排空</p>
        <p className="text-[17px] mt-1 font-medium" style={{ ...serif, color: "var(--fg-strong)" }}>业务池出口一览</p>
      </div>
      {bindings.length === 0 ? (
        <p className="text-sm py-6 text-center" style={{ ...sans, color: "var(--muted)" }}>暂无生效的出口绑定</p>
      ) : (
        <div>
          {bindings.map((binding, index) => {
            const drain = drainByPool.get(binding.pool);
            const isDirect = binding.target.kind === "direct" || !binding.target.smart_node;
            return (
              <div
                key={binding.id}
                className="py-3.5 first:pt-0"
                style={index > 0 ? { borderTop: "1px solid color-mix(in srgb, var(--border) 60%, transparent)" } : undefined}
              >
                <div className="flex items-center justify-between gap-2 flex-wrap">
                  <div className="flex gap-1.5 flex-wrap">
                    <Badge tone="accent">{poolLabel(binding.pool)}</Badge>
                    <Badge tone={drain ? "warn" : "ok"}>线路版本 {binding.generation}</Badge>
                  </div>
                  <span className="text-[11px] whitespace-nowrap" style={{ ...sans, color: "var(--muted)" }}>
                    生效于 {formatAgo(binding.since)}
                  </span>
                </div>
                <p className="text-[13px] mt-1.5" style={{ ...sans, color: "var(--fg-strong)" }}>
                  {isDirect ? "直连出口（不经代理）" : nodeLabel(binding.target.smart_node ?? "")}
                </p>
                {!isDirect ? (
                  <p className="text-[10.5px] mt-0.5 break-all leading-relaxed" style={{ ...mono, color: "var(--muted)" }}>
                    {binding.target.smart_node}{binding.target.worker ? ` · ${binding.target.worker}` : ""}
                  </p>
                ) : null}
                {drain ? <DrainBlock drain={drain} /> : null}
              </div>
            );
          })}
        </div>
      )}
    </Card>
  );
}

function DrainBlock({ drain }: { drain: DrainProgress }) {
  const deadline = drain.deadline ? new Date(drain.deadline) : null;
  const deadlineText = deadline && !Number.isNaN(deadline.getTime())
    ? deadline.toLocaleTimeString("zh-CN", { hour12: false, hour: "2-digit", minute: "2-digit" })
    : null;
  return (
    <div
      className="mt-2.5 px-3.5 py-3"
      style={{
        borderRadius: "var(--radius-sm)",
        background: "color-mix(in srgb, var(--warning) 8%, transparent)",
        border: "1px solid color-mix(in srgb, var(--warning) 25%, transparent)",
      }}
    >
      <div className="flex items-baseline justify-between gap-2 text-xs" style={{ ...sans, color: "var(--fg)" }}>
        <Badge tone={drain.status === "failed" ? "err" : "warn"}>
          {drainStatusLabel(drain.status)} · 旧线路版本 {drain.old_generation}
        </Badge>
        <span style={mono}>剩余 {drain.remaining} 条</span>
      </div>
      <p className="text-[11px] mt-1.5 leading-relaxed" style={{ ...sans, color: "var(--muted)" }}>
        {drain.status === "retrying"
          ? `第 ${drain.retry_count} 次重试清理中`
          : drain.status === "failed"
            ? drain.last_error || "清理失败，等待下次重试"
            : "等待旧线路连接自然结束"}
        {deadlineText ? ` · 截止 ${deadlineText}` : ""}
      </p>
    </div>
  );
}

/* ═══════════ 业务池 / 协议分布 ═══════════ */

function PoolDistribution({
  connections,
  activePool,
  poolLabel,
  onSelect,
}: {
  connections: ConnectionItem[];
  activePool: string;
  poolLabel: (id: string) => string;
  onSelect: (pool: string) => void;
}) {
  const poolRows = useMemo(() => {
    const counts = new Map<string, number>();
    for (const connection of connections) {
      const key = connection.pool || "";
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]);
  }, [connections]);
  const protoRows = useMemo(() => {
    const counts = new Map<string, number>();
    for (const connection of connections) {
      const key = String(connection.metadata?.network || "其他").toUpperCase();
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]);
  }, [connections]);
  const poolMax = Math.max(...poolRows.map(([, count]) => count), 1);
  const protoMax = Math.max(...protoRows.map(([, count]) => count), 1);

  return (
    <Card className="h-full" style={{ padding: 28 }}>
      <div className="mb-4">
        <p style={eyebrowStyle}>分布</p>
        <p className="text-[17px] mt-1 font-medium" style={{ ...serif, color: "var(--fg-strong)" }}>按业务池</p>
        <p className="text-[11.5px] mt-1" style={{ ...sans, color: "var(--muted)" }}>点击条目可筛选下方列表</p>
      </div>
      {poolRows.length === 0 ? (
        <p className="text-sm py-4 text-center" style={{ ...sans, color: "var(--muted)" }}>暂无连接</p>
      ) : (
        poolRows.map(([pool, count]) => (
          <DistributionBar
            key={pool || "unassigned"}
            label={pool ? poolLabel(pool) : "未归属"}
            count={count}
            ratio={count / poolMax}
            active={activePool === pool}
            onClick={pool ? () => onSelect(pool) : undefined}
          />
        ))
      )}
      {protoRows.length > 0 ? (
        <>
          <hr className="my-4" style={{ border: "none", borderTop: "1px solid var(--border)", opacity: 0.6 }} />
          <p style={eyebrowStyle} className="mb-2">协议</p>
          {protoRows.map(([proto, count]) => (
            <DistributionBar key={proto} label={proto} count={count} ratio={count / protoMax} dimmed />
          ))}
        </>
      ) : null}
    </Card>
  );
}

function DistributionBar({
  label,
  count,
  ratio,
  active,
  dimmed,
  onClick,
}: {
  label: string;
  count: number;
  ratio: number;
  active?: boolean;
  dimmed?: boolean;
  onClick?: () => void;
}) {
  const Tag = onClick ? "button" : "div";
  return (
    <Tag
      onClick={onClick}
      className={`grid items-center gap-2.5 w-full text-left py-1.5 px-1.5 -mx-1.5 rounded transition-colors ${onClick ? "cursor-pointer hover:bg-[var(--surface-2)]" : ""}`}
      style={{ gridTemplateColumns: "84px minmax(0,1fr) 36px" }}
      title={onClick ? `点击筛选「${label}」` : undefined}
      type={onClick ? "button" : undefined}
    >
      <span
        className="text-[13px] truncate"
        style={{ ...sans, color: active ? "var(--accent)" : "var(--fg-strong)", fontWeight: active ? 600 : 400 }}
      >
        {label}
      </span>
      <span className="h-2.5 relative">
        <span
          className="block h-full transition-all duration-300"
          style={{
            width: `${Math.max(2, ratio * 100)}%`,
            borderRadius: "0 4px 4px 0",
            background: dimmed ? "color-mix(in srgb, var(--accent) 60%, transparent)" : "var(--accent)",
          }}
        />
      </span>
      <span className="text-xs text-right tabular-nums" style={{ ...mono, color: "var(--fg)" }}>{count}</span>
    </Tag>
  );
}

/* ═══════════ 流量 Top 5 站点 ═══════════ */

function TopHosts({ connections }: { connections: ConnectionItem[] }) {
  const rows = useMemo(() => {
    const totals = new Map<string, number>();
    for (const connection of connections) {
      const host = String(connection.metadata?.host || connection.metadata?.destinationIP || connection.id);
      totals.set(host, (totals.get(host) ?? 0) + connection.download + connection.upload);
    }
    return [...totals.entries()].sort((a, b) => b[1] - a[1]).slice(0, 5);
  }, [connections]);
  const max = Math.max(...rows.map(([, total]) => total), 1);

  return (
    <Card className="h-full" style={{ padding: 28 }}>
      <div className="mb-4">
        <p style={eyebrowStyle}>目标站点</p>
        <p className="text-[17px] mt-1 font-medium" style={{ ...serif, color: "var(--fg-strong)" }}>流量 Top 5</p>
      </div>
      {rows.length === 0 ? (
        <p className="text-sm py-4 text-center" style={{ ...sans, color: "var(--muted)" }}>暂无连接</p>
      ) : (
        rows.map(([host, total], index) => (
          <div
            key={host}
            className="flex flex-col gap-1 py-2.5 first:pt-0 last:pb-0"
            style={index > 0 ? { borderTop: "1px solid color-mix(in srgb, var(--border) 60%, transparent)" } : undefined}
            title={host}
          >
            <div className="flex items-baseline justify-between gap-2.5">
              <span className="text-[14px] truncate" style={{ ...serif, color: "var(--fg-strong)" }}>
                <span className="text-[11px] mr-2" style={{ ...mono, color: "var(--muted)" }}>0{index + 1}</span>
                {host}
              </span>
              <span className="text-[11.5px] whitespace-nowrap tabular-nums" style={{ ...mono, color: "var(--muted)" }}>
                {formatBytes(total)}
              </span>
            </div>
            <div
              className="h-1 transition-all duration-300"
              style={{
                width: `${Math.max(2, (total / max) * 100)}%`,
                borderRadius: "0 2px 2px 0",
                background: "color-mix(in srgb, var(--accent) 55%, transparent)",
              }}
            />
          </div>
        ))
      )}
    </Card>
  );
}

/* ═══════════ 连接表 ═══════════ */

function ConnectionsTable({
  connections,
  closingID,
  connRates,
  poolLabel,
  nodeLabel,
  chainLabel,
  coreLabels,
  onClose,
}: {
  connections: ConnectionItem[];
  closingID: string;
  connRates: Map<string, RateSample>;
  poolLabel: (id: string) => string;
  nodeLabel: (id: string) => string;
  chainLabel: (entry: string) => string;
  coreLabels: CoreLabelMap;
  onClose: (connection: ConnectionItem) => void;
}) {
  return (
    <Card>
      <div className="overflow-x-auto">
        <table className="w-full text-sm min-w-[1080px]" style={{ ...sans, color: "var(--fg)" }}>
          <thead>
            <tr style={{ borderBottom: "1px solid var(--border-strong)" }}>
              <Th>目标</Th>
              <Th>业务池 · 线路</Th>
              <Th>节点</Th>
              <Th>执行核心</Th>
              <Th>转发路径</Th>
              <Th align="right">流量</Th>
              <Th align="right">操作</Th>
            </tr>
          </thead>
          <tbody>
            {connections.map((connection) => (
              <ConnectionRow
                key={connection.id}
                connection={connection}
                rate={connRates.get(connection.id)}
                poolLabel={poolLabel}
                nodeLabel={nodeLabel}
                chainLabel={chainLabel}
                coreLabels={coreLabels}
                closing={closingID === connection.id}
                onClose={() => onClose(connection)}
              />
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function ConnectionRow({
  connection,
  rate,
  poolLabel,
  nodeLabel,
  chainLabel,
  coreLabels,
  closing,
  onClose,
}: {
  connection: ConnectionItem;
  rate: RateSample | undefined;
  poolLabel: (id: string) => string;
  nodeLabel: (id: string) => string;
  chainLabel: (entry: string) => string;
  coreLabels: CoreLabelMap;
  closing: boolean;
  onClose: () => void;
}) {
  const metadata = connection.metadata ?? {};
  const dnsHosts = Array.isArray(metadata.dnsHosts) ? metadata.dnsHosts.filter((name): name is string => typeof name === "string" && name.length > 0) : [];
  const reportedHost = metadata.host && metadata.host !== metadata.destinationIP ? metadata.host : "";
  const destination = metadata.destinationIP || "未知 IP";
  const endpoint = `${destination.includes(":") ? `[${destination}]` : destination}${metadata.destinationPort ? `:${metadata.destinationPort}` : ""}`;
  const host = reportedHost || dnsHosts[0] || endpoint;
  const forwardingPath = connection.chains.length ? connection.chains.map(chainLabel) : [
    connection.pool ? poolLabel(connection.pool) : "未识别业务池",
    connection.core ? coreLabel(connection.core, coreLabels) : "",
    connection.smart_node ? nodeLabel(connection.smart_node) : "",
  ].filter(Boolean);
  const duration = formatDuration(connection.start);
  return (
    <tr
      style={{
        borderBottom: "1px solid var(--border)",
        background: connection.stale_generation
          ? "color-mix(in srgb, var(--warning) 6%, transparent)"
          : "transparent",
      }}
    >
      <Td>
        <div className="text-[15px] max-w-[260px] truncate" title={[String(host), ...dnsHosts].filter((value, index, values) => values.indexOf(value) === index).join("、")} style={{ ...serif, color: "var(--fg-strong)" }}>{String(host)}</div>
        {reportedHost || dnsHosts.length ? (
          <div className="text-[11px] mt-1 break-all" style={{ ...mono, color: "var(--muted)" }}>{endpoint}</div>
        ) : null}
        {!reportedHost && dnsHosts.length > 0 ? (
          <div className="text-[11px] mt-1" style={{ color: "var(--muted)" }} title={`近期 DNS 关联：${dnsHosts.join("、")}。共享 IP 的域名不一定是此连接实际使用的域名。`}>
            DNS 关联{dnsHosts.length > 1 ? ` · ${dnsHosts.length} 个候选域名` : ""}
          </div>
        ) : null}
        <div className="text-[11px] mt-1" style={{ ...mono, color: "var(--muted)" }}>
          {metadata.sourceIP ? `${metadata.sourceIP}:${metadata.sourcePort || "-"}` : connection.id}
          {duration ? ` · 已持续 ${duration}` : ""}
        </div>
        <div className="mt-1.5">
          <Badge tone="muted">{String(metadata.network || metadata.type || "未知协议").toUpperCase()}</Badge>
        </div>
      </Td>
      <Td>
        <div className="flex items-center gap-1.5 flex-wrap">
          <Badge tone="accent">{poolLabel(connection.pool)}</Badge>
          <Badge tone={connection.stale_generation ? "warn" : "ok"}>
            <span title={`线路版本 ${connection.generation}：业务池切换节点时递增，用于区分切换前后的连接。`}>
              {connection.stale_generation ? "旧线路" : "当前线路"}
            </span>
          </Badge>
        </div>
        {connection.stale_generation ? (
          <p className="text-xs mt-1.5" style={{ color: "var(--warning)" }}>切换前的连接 · 排空中</p>
        ) : null}
      </Td>
      <Td>
        {connection.smart_node ? (
          <>
            <div style={{ color: "var(--fg-strong)" }}>{nodeLabel(connection.smart_node)}</div>
            <div className="text-[10.5px] mt-1 break-all leading-relaxed" style={{ ...mono, color: "var(--muted)" }}>
              {connection.smart_node}
            </div>
          </>
        ) : (
          <span style={{ color: "var(--muted)" }}>未关联代理节点</span>
        )}
      </Td>
      <Td>
        {connection.worker ? (
          <>
            <CoreBadge core={connection.core} coreLabels={coreLabels} />
            <div className="text-[10.5px] mt-1.5 break-all leading-relaxed" style={{ ...mono, color: "var(--muted)" }}>
              {connection.worker}
            </div>
          </>
        ) : (
          <span style={{ color: "var(--muted)" }}>—</span>
        )}
      </Td>
      <Td>
        <div className="text-[13px] leading-[1.9] max-w-[260px]" style={{ ...sans, color: "var(--fg-strong)" }}>
          {forwardingPath.map((entry, index) => (
            <span key={`${entry}-${index}`}>
              {index > 0 ? <span style={{ color: "var(--muted)" }}>{" → "}</span> : null}
              {entry}
            </span>
          ))}
          <div className="text-[11px] mt-1" style={{ color: "var(--muted)" }}>
            {connection.chains.length ? "核心上报" : "按业务池线路关联"}
          </div>
        </div>
      </Td>
      <Td align="right" mono>
        <div className="whitespace-nowrap">↓ {formatBytes(connection.download)}</div>
        <div className="mt-1 whitespace-nowrap" style={{ color: "var(--muted)" }}>↑ {formatBytes(connection.upload)}</div>
        {rate ? (
          <div className="text-[11px] mt-1 whitespace-nowrap" style={{ color: "var(--muted)" }}>
            实时 ↓ {formatRate(rate.d)}
          </div>
        ) : null}
      </Td>
      <Td align="right">
        <Button size="sm" variant="danger" loading={closing} onClick={onClose}>关闭此连接</Button>
      </Td>
    </tr>
  );
}

function CoreBadge({ core, coreLabels }: { core: CoreID | ""; coreLabels: CoreLabelMap }) {
  if (!core) return <Badge tone="muted">无核心</Badge>;
  return <Badge tone="info">{coreLabel(core, coreLabels)}</Badge>;
}

function CloseResultNotice({
  result,
  poolLabel,
}: {
  result: ConnectionCloseResult;
  poolLabel: (id: string) => string;
}) {
  const label = result.scope === "single"
    ? "单条连接"
    : result.scope === "binding"
      ? `业务池「${poolLabel(result.pool ?? "")}」`
      : "全局诊断";
  return (
    <Card
      style={{
        borderColor: result.closed_exact
          ? "color-mix(in srgb, var(--success) 40%, var(--border))"
          : "color-mix(in srgb, var(--warning) 45%, var(--border))",
      }}
    >
      <div className="px-6 py-4 flex items-center gap-3 flex-wrap">
        <Badge tone={result.closed_exact ? "ok" : "warn"}>{label}</Badge>
        <p className="text-sm" style={{ ...sans, color: "var(--fg-strong)" }}>
          {result.closed_exact
            ? `已精确关闭 ${result.closed} 条连接。`
            : "关闭请求已执行，但当前后端无法精确核验关闭数量；显示的 0 不是已确认计数。"}
        </p>
      </div>
    </Card>
  );
}

function ConfirmCloseModal({
  target,
  poolLabel,
  loading,
  error,
  onClose,
  onConfirm,
}: {
  target: CloseTarget;
  poolLabel: (id: string) => string;
  loading: boolean;
  error: unknown;
  onClose: () => void;
  onConfirm: () => void;
}) {
  const [confirmation, setConfirmation] = useState("");
  const expected = target?.scope === "binding" ? target.pool : "confirm";

  useEffect(() => {
    setConfirmation("");
  }, [target]);

  if (!target) return null;

  const binding = target.scope === "binding";
  return (
    <Modal
      open
      onClose={onClose}
      eyebrow="破坏性连接操作"
      title={binding ? `关闭「${poolLabel(target.pool)}」的全部连接` : "关闭全部观测连接"}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button
            variant="danger"
            onClick={onConfirm}
            disabled={confirmation.trim() !== expected || loading}
            loading={loading}
          >
            确认关闭
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <p className="text-[15px] leading-7" style={{ ...serif, color: "var(--fg)" }}>
          {binding
            ? "这会关闭该业务池当前线路与排空中旧线路的所有连接，不影响其他业务池。"
            : "全局关闭仅用于诊断，会影响所有业务池；SmartGate 的正常切换流程不依赖此操作。"}
        </p>
        <Field
          label={binding ? `输入业务池 ID「${expected}」确认` : "输入 confirm 确认"}
          error={error ? <ErrorText error={error} fallback="关闭连接失败" showDiagnostic /> : undefined}
        >
          <Input
            value={confirmation}
            onChange={(event) => setConfirmation(event.target.value)}
            placeholder={expected}
            autoFocus
          />
        </Field>
      </div>
    </Modal>
  );
}

function Th({ children, align = "left" }: { children: React.ReactNode; align?: "left" | "right" }) {
  return (
    <th
      className="px-5 py-3.5 text-[11px]"
      style={{
        color: "var(--muted)",
        textAlign: align,
        textTransform: "uppercase",
        letterSpacing: "0.16em",
        whiteSpace: "nowrap",
      }}
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align = "left",
  mono: isMono,
}: {
  children: React.ReactNode;
  align?: "left" | "right";
  mono?: boolean;
}) {
  return (
    <td
      className="px-5 py-4 align-top"
      style={{ textAlign: align, fontFamily: isMono ? "var(--font-mono)" : undefined }}
    >
      {children}
    </td>
  );
}

/* ═══════════ 格式化工具 ═══════════ */

function formatBytes(value: number): string {
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

function formatRate(value: number): string {
  return `${formatBytes(value)}/s`;
}

function splitRate(value: number | undefined): { value: string; unit: string } {
  if (value === undefined) return { value: "—", unit: "" };
  const [num, unit] = formatRate(value).split(" ");
  return { value: num ?? "0", unit: unit ?? "" };
}

function splitBytes(value: number): { value: string; unit: string } {
  const [num, unit] = formatBytes(value).split(" ");
  return { value: num ?? "0", unit: unit ?? "" };
}

function formatDuration(startISO: string | undefined): string {
  if (!startISO) return "";
  const start = new Date(startISO).getTime();
  if (Number.isNaN(start)) return "";
  const seconds = Math.max(0, (Date.now() - start) / 1000);
  if (seconds < 60) return `${Math.round(seconds)} 秒`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
  return `${(seconds / 3600).toFixed(1)} 小时`;
}

function formatAgo(sinceISO: string): string {
  const since = new Date(sinceISO).getTime();
  if (Number.isNaN(since)) return "未知时间";
  const seconds = Math.max(0, (Date.now() - since) / 1000);
  if (seconds < 90) return `${Math.round(seconds)} 秒前`;
  if (seconds < 5400) return `${Math.round(seconds / 60)} 分钟前`;
  return `${(seconds / 3600).toFixed(1)} 小时前`;
}
