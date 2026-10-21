import { useMemo, useState, type CSSProperties, type ReactNode } from "react";
import {
  useWifiStatus,
  useWifiScan,
  useWifiClients,
  useWifiConfig,
  useWifiConfigUpdate,
} from "../lib/queries";
import type {
  WifiClient,
  WifiConfigNetwork,
  WifiConfigUpdate,
  WifiNetwork,
  WifiRadio,
} from "../lib/types";
import { Card, SectionHeader, Eyebrow, Rule } from "../ui/Card";
import { Badge, Dot } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Field, Input, Select } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { Segmented, Toggle } from "../ui/Segmented";
import { serif, sans } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

type Tab = "status" | "clients" | "scan" | "config";
type Tone = "ok" | "warn" | "err" | "info" | "muted" | "accent";

export function WifiPage() {
  const [tab, setTab] = useState<Tab>("status");
  return (
    <div className="space-y-12 max-w-[1120px]">
      <SectionHeader
        eyebrow="网络 · Wi-Fi"
        title="无线网络管理"
        desc="查看射频状态、已连接设备、扫描周边网络，并安全地修改 SSID / 密码。"
      />
      <Segmented<Tab>
        value={tab}
        onChange={setTab}
        options={[
          { key: "status", label: "状态", title: "无线状态" },
          { key: "clients", label: "客户端", title: "已连接设备" },
          { key: "scan", label: "扫描", title: "周边 Wi-Fi 网络" },
          { key: "config", label: "配置", title: "修改 SSID / 密码" },
        ]}
      />
      {tab === "status" ? <StatusTab /> : null}
      {tab === "clients" ? <ClientsTab /> : null}
      {tab === "scan" ? <ScanTab /> : null}
      {tab === "config" ? <ConfigTab /> : null}
      <Eyebrow>SmartGate · 书房 · 网络 · Wi-Fi</Eyebrow>
    </div>
  );
}

function StatusTab() {
  const status = useWifiStatus();
  const radios = status.data?.radios ?? [];
  const ifaces = status.data?.interfaces ?? [];
  const upCount = radios.filter((r) => r.up && !r.disabled).length;
  const ssidCount = new Set(ifaces.map((i) => i.ssid).filter(Boolean)).size;

  return (
    <section className="space-y-6">
      <SectionHeader
        eyebrow="状态"
        title="无线网络状态"
        desc="按射频与接口展示运行情况，不再直接 dump ubus 原始输出。"
        right={
          status.isLoading ? (
            <Badge tone="muted">载入中…</Badge>
          ) : status.error ? (
            <Badge tone="err"><ErrorText error={status.error} fallback="无线状态加载失败" /></Badge>
          ) : (
            <div className="flex items-center gap-2 flex-wrap">
              <Badge tone={upCount > 0 ? "ok" : "warn"}>
                <Dot tone={upCount > 0 ? "ok" : "warn"} />
                {upCount}/{radios.length || 0} 射频在线
              </Badge>
              <Badge tone="info">{ssidCount} 个 SSID</Badge>
              <Badge tone="muted">{ifaces.length} 个接口</Badge>
            </div>
          )
        }
      />

      {status.error && !status.data ? (
        <Card>
          <div className="px-7 py-8 text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={status.error} fallback="无线状态加载失败" />
          </div>
        </Card>
      ) : null}

      {status.data && radios.length === 0 ? (
        <Card>
          <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
            未发现无线射频。请确认 OpenWrt 无线驱动与 hostapd 已启动。
          </div>
        </Card>
      ) : null}

      <div className="grid gap-4 md:grid-cols-2">
        {radios.map((radio) => (
          <RadioCard key={radio.id} radio={radio} />
        ))}
      </div>

      {ifaces.length > 0 ? (
        <>
          <Rule />
          <SectionHeader eyebrow="接口" title="AP 接口一览" />
          <Card>
            <ul>
              {ifaces.map((iface, i) => (
                <li
                  key={`${iface.section}-${iface.ifname || i}`}
                  className="px-7 py-4 flex items-start justify-between gap-4"
                  style={{
                    borderBottom: i === ifaces.length - 1 ? "none" : "1px solid var(--border)",
                  }}
                >
                  <div className="min-w-0">
                    <p className="text-[16px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                      {iface.ssid || "（未命名 SSID）"}
                    </p>
                    <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
                      {iface.ifname || "—"} · section {iface.section}
                      {iface.mode ? ` · ${iface.mode}` : ""}
                      {iface.encryption ? ` · ${iface.encryption}` : ""}
                      {iface.network ? ` · 网络 ${iface.network}` : ""}
                    </p>
                  </div>
                  <div className="flex items-center gap-2 flex-wrap justify-end">
                    <Badge tone="accent">{bandLabel(iface.band)}</Badge>
                    {iface.channel ? <Badge tone="muted">CH {iface.channel}</Badge> : null}
                    <Badge tone={iface.disabled ? "warn" : "ok"}>
                      {iface.disabled ? "已禁用" : "启用"}
                    </Badge>
                  </div>
                </li>
              ))}
            </ul>
          </Card>
        </>
      ) : null}
    </section>
  );
}

function RadioCard({ radio }: { radio: WifiRadio }) {
  const tone: Tone = radio.disabled ? "warn" : radio.up ? "ok" : "muted";
  return (
    <Card inset>
      <div className="flex items-start justify-between gap-3">
        <div>
          <p style={eyebrowMini}>{radio.id}</p>
          <p className="text-[20px] mt-1" style={{ ...serif, color: "var(--fg-strong)" }}>
            {bandLabel(radio.band)} 射频
          </p>
        </div>
        <Badge tone={tone}>
          <Dot tone={tone} />
          {radio.disabled ? "已禁用" : radio.up ? "运行中" : radio.pending ? "准备中" : "离线"}
        </Badge>
      </div>
      <div className="mt-5 grid grid-cols-2 gap-3 text-sm" style={{ ...sans, color: "var(--fg)" }}>
        <Meta label="频段" value={bandLabel(radio.band)} />
        <Meta label="信道" value={radio.channel || "—"} />
        <Meta label="带宽模式" value={radio.htmode || "—"} />
        <Meta label="国家码" value={radio.country || "—"} />
        <Meta label="发射功率" value={radio.txpower ? `${radio.txpower} dBm` : "—"} />
        <Meta label="接口数" value={String(radio.interfaces?.length ?? 0)} />
      </div>
      {(radio.interfaces || []).length > 0 ? (
        <div className="mt-5 space-y-2">
          {(radio.interfaces || []).map((iface) => (
            <div
              key={`${radio.id}-${iface.section}-${iface.ifname}`}
              className="px-3 py-2 rounded-[8px] flex items-center justify-between gap-2"
              style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}
            >
              <div className="min-w-0">
                <p className="text-sm truncate" style={{ ...sans, color: "var(--fg-strong)" }}>
                  {iface.ssid || iface.section}
                </p>
                <p className="text-[11px]" style={{ ...sans, color: "var(--muted)" }}>
                  {iface.ifname || "无 ifname"}
                </p>
              </div>
              <Badge tone={iface.disabled ? "warn" : "ok"}>{iface.disabled ? "关" : "开"}</Badge>
            </div>
          ))}
        </div>
      ) : null}
    </Card>
  );
}

function ClientsTab() {
  const status = useWifiStatus();
  const ifaceOptions = useMemo(() => {
    const list = (status.data?.interfaces || [])
      .map((i) => i.ifname)
      .filter((v): v is string => Boolean(v));
    return ["all", ...Array.from(new Set(list))];
  }, [status.data]);
  const [iface, setIface] = useState("all");
  const clients = useWifiClients(iface);
  const rows = clients.data?.clients ?? [];

  return (
    <section className="space-y-6">
      <SectionHeader
        eyebrow="客户端"
        title={clients.data ? `${rows.length} 台已连接设备` : "已连接设备"}
        desc="聚合 hostapd / iwinfo 关联站，并用 DHCP 租约补全主机名与 IP。"
        right={
          clients.isLoading ? (
            <Badge tone="muted">载入中…</Badge>
          ) : clients.error ? (
            <Badge tone="err"><ErrorText error={clients.error} fallback="无线客户端加载失败" /></Badge>
          ) : (
            <Badge tone="ok">
              <Dot tone="ok" />
              在线 {rows.length}
            </Badge>
          )
        }
      />

      <div className="flex items-end gap-3 flex-wrap">
        <Field label="接口范围">
          <Select value={iface} onChange={(e) => setIface(e.target.value)} style={{ minWidth: 220 }}>
            {ifaceOptions.map((opt) => (
              <option key={opt} value={opt}>
                {opt === "all" ? "全部接口" : opt}
              </option>
            ))}
          </Select>
        </Field>
        <Button variant="ghost" size="sm" onClick={() => clients.refetch()} loading={clients.isFetching}>
          刷新
        </Button>
      </div>

      {clients.error && !clients.data ? (
        <Card>
          <div className="px-7 py-8 text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={clients.error} fallback="无线客户端加载失败" />
          </div>
        </Card>
      ) : rows.length === 0 ? (
        <Card>
          <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
            当前范围暂无已关联客户端。
          </div>
        </Card>
      ) : (
        <Card>
          <div className="overflow-x-auto">
            <table className="w-full text-sm min-w-[720px]" style={{ ...sans, color: "var(--fg)" }}>
              <thead>
                <tr style={{ borderBottom: "1px solid var(--border-strong)" }}>
                  <Th>设备</Th>
                  <Th>地址</Th>
                  <Th>接入</Th>
                  <Th align="right">信号</Th>
                  <Th align="right">速率</Th>
                </tr>
              </thead>
              <tbody>
                {rows.map((c) => (
                  <ClientRow key={`${c.mac}-${c.interface || ""}`} c={c} />
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </section>
  );
}

function ClientRow({ c }: { c: WifiClient }) {
  const title = c.hostname || c.mac;
  return (
    <tr style={{ borderBottom: "1px solid var(--border)" }}>
      <Td>
        <p style={{ color: "var(--fg-strong)", fontWeight: 500 }}>{title}</p>
        {c.hostname ? (
          <p className="text-[11px] mt-0.5" style={{ color: "var(--muted)" }}>
            {c.mac}
          </p>
        ) : null}
      </Td>
      <Td>
        <p>{c.ip || "—"}</p>
        <p className="text-[11px] mt-0.5" style={{ color: "var(--muted)" }}>
          {c.authorized === false ? "未授权" : "已关联"}
        </p>
      </Td>
      <Td>
        <div className="flex flex-col gap-1 items-start">
          <Badge tone="accent">{bandFromIfname(c.interface) || c.ssid || "Wi-Fi"}</Badge>
          <span className="text-[11px]" style={{ color: "var(--muted)" }}>
            {[c.interface, c.ssid].filter(Boolean).join(" · ") || "—"}
          </span>
        </div>
      </Td>
      <Td align="right">
        <Badge tone={signalTone(c.signal_dbm)}>{formatSignal(c.signal_dbm)}</Badge>
      </Td>
      <Td align="right">
        <p>{formatRatePair(c.rx_rate, c.tx_rate)}</p>
        {c.rx_bytes || c.tx_bytes ? (
          <p className="text-[11px] mt-0.5" style={{ color: "var(--muted)" }}>
            ↓ {formatBytes(c.rx_bytes || 0)} · ↑ {formatBytes(c.tx_bytes || 0)}
          </p>
        ) : null}
      </Td>
    </tr>
  );
}

function ScanTab() {
  const status = useWifiStatus();
  const scan = useWifiScan();
  const ifaceChoices = useMemo(() => {
    const list = (status.data?.interfaces || [])
      .map((i) => i.ifname)
      .filter((v): v is string => Boolean(v));
    return ["auto", ...Array.from(new Set(list))];
  }, [status.data]);
  const [iface, setIface] = useState("auto");
  const networks = scan.data?.networks ?? [];

  return (
    <section className="space-y-6">
      <SectionHeader
        eyebrow="扫描"
        title={scan.data ? `发现 ${networks.length} 个周边网络` : "周边 Wi-Fi 网络"}
        desc="在 AP 接口上执行被动扫描。接口默认自动选择第一个可用射频。"
        right={
          scan.isPending ? (
            <Badge tone="muted">扫描中…</Badge>
          ) : scan.error ? (
            <Badge tone="err"><ErrorText error={scan.error} fallback="无线网络扫描失败" /></Badge>
          ) : scan.data ? (
            <Badge tone="info">接口 {scan.data.interface}</Badge>
          ) : null
        }
      />

      <div className="flex items-end gap-3 flex-wrap">
        <Field label="扫描接口" hint="推荐 auto">
          <Select value={iface} onChange={(e) => setIface(e.target.value)} style={{ minWidth: 220 }}>
            {ifaceChoices.map((opt) => (
              <option key={opt} value={opt}>
                {opt === "auto" ? "自动选择" : opt}
              </option>
            ))}
          </Select>
        </Field>
        <Button onClick={() => scan.mutateAsync(iface)} loading={scan.isPending}>
          开始扫描
        </Button>
      </div>

      {scan.error && !scan.data ? (
        <Card>
          <div className="px-7 py-8 text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={scan.error} fallback="无线网络扫描失败" />
          </div>
        </Card>
      ) : null}

      {scan.data && networks.length === 0 ? (
        <Card>
          <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
            未扫描到周边网络（接口 {scan.data.interface}）。
          </div>
        </Card>
      ) : null}

      {networks.length > 0 ? (
        <Card>
          <ul>
            {networks.map((n, i) => (
              <ScanRow key={`${n.bssid}-${n.ssid}-${i}`} n={n} last={i === networks.length - 1} />
            ))}
          </ul>
        </Card>
      ) : !scan.data && !scan.error ? (
        <Card>
          <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
            点击「开始扫描」查看周边 SSID。扫描期间本机射频可能短暂繁忙。
          </div>
        </Card>
      ) : null}
    </section>
  );
}

function ScanRow({ n, last }: { n: WifiNetwork; last: boolean }) {
  return (
    <li
      className="px-7 py-4 flex items-start justify-between gap-4"
      style={{ borderBottom: last ? "none" : "1px solid var(--border)" }}
    >
      <div className="min-w-0">
        <p className="text-[16px]" style={{ ...serif, color: "var(--fg-strong)" }}>
          {n.ssid || "（隐藏 SSID）"}
        </p>
        <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
          {n.bssid || "—"}
          {n.encryption ? ` · ${n.encryption}` : ""}
          {n.quality ? ` · 质量 ${n.quality}` : ""}
        </p>
      </div>
      <div className="flex items-center gap-2 flex-wrap justify-end">
        {n.channel ? <Badge tone="muted">CH {n.channel}</Badge> : null}
        <Badge tone={signalTone(n.signal_dbm)}>{formatSignal(n.signal_dbm)}</Badge>
      </div>
    </li>
  );
}

function ConfigTab() {
  const config = useWifiConfig();
  const update = useWifiConfigUpdate();
  const networks = config.data?.networks ?? [];
  const devices = config.data?.devices ?? [];
  const [editing, setEditing] = useState<WifiConfigNetwork | null>(null);

  return (
    <section className="space-y-6">
      <SectionHeader
        eyebrow="配置"
        title="无线网络配置"
        desc="以 SSID 为单位编辑名称、加密与密码。应用后会 commit wireless 并 reload，无线会短暂中断。"
        right={
          config.isLoading ? (
            <Badge tone="muted">载入中…</Badge>
          ) : config.error ? (
            <Badge tone="err"><ErrorText error={config.error} fallback="无线配置加载失败" /></Badge>
          ) : (
            <Badge tone="info">{networks.length} 个网络</Badge>
          )
        }
      />

      {config.error && !config.data ? (
        <Card>
          <div className="px-7 py-8 text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={config.error} fallback="无线配置加载失败" />
          </div>
        </Card>
      ) : null}

      {networks.length === 0 && config.data ? (
        <Card>
          <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
            未解析到 wifi-iface 网络。
          </div>
        </Card>
      ) : (
        <div className="grid gap-4">
          {networks.map((net) => (
            <Card key={net.section} inset>
              <div className="flex items-start justify-between gap-4 flex-wrap">
                <div className="min-w-0">
                  <p style={eyebrowMini}>{net.section}</p>
                  <p className="text-[22px] mt-1" style={{ ...serif, color: "var(--fg-strong)" }}>
                    {net.ssid || "（未命名）"}
                  </p>
                  <p className="text-xs mt-2" style={{ ...sans, color: "var(--muted)" }}>
                    {[
                      net.device,
                      bandLabel(net.band),
                      net.channel ? `CH ${net.channel}` : "",
                      net.htmode,
                      net.encryption,
                      net.mode,
                      net.network ? `网络 ${net.network}` : "",
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </p>
                </div>
                <div className="flex items-center gap-2">
                  <Badge tone={net.disabled ? "warn" : "ok"}>
                    {net.disabled ? "已禁用" : "已启用"}
                  </Badge>
                  <Badge tone="muted">{net.key_configured ? "已设密码" : "未设密码"}</Badge>
                  <Button size="sm" variant="ghost" onClick={() => setEditing(net)}>
                    编辑
                  </Button>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}

      {devices.length > 0 ? (
        <>
          <Rule />
          <SectionHeader
            eyebrow="射频设备"
            title="wifi-device 摘要"
            desc="只读展示；信道 / 功率改动请走 UCI 或后续高级编辑。"
          />
          <div className="grid gap-3 md:grid-cols-2">
            {devices.map((d) => (
              <Card key={d.section} inset>
                <div className="flex items-center justify-between gap-2">
                  <p className="text-[16px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                    {d.section}
                  </p>
                  <Badge tone={d.disabled ? "warn" : "ok"}>{d.disabled ? "禁用" : "启用"}</Badge>
                </div>
                <div className="mt-4 grid grid-cols-2 gap-2 text-sm" style={{ ...sans, color: "var(--fg)" }}>
                  <Meta label="频段" value={bandLabel(d.band)} />
                  <Meta label="信道" value={d.channel || "—"} />
                  <Meta label="模式" value={d.htmode || "—"} />
                  <Meta label="国家" value={d.country || "—"} />
                  <Meta label="功率" value={d.txpower || "—"} />
                </div>
              </Card>
            ))}
          </div>
        </>
      ) : null}

      {editing ? (
        <ConfigEditModal
          network={editing}
          loading={update.isPending}
          error={update.error}
          onClose={() => {
            if (!update.isPending) setEditing(null);
          }}
          onSave={async (payload) => {
            await update.mutateAsync({ ...payload, confirm: true });
            setEditing(null);
          }}
        />
      ) : null}
    </section>
  );
}

function ConfigEditModal({
  network,
  loading,
  error,
  onClose,
  onSave,
}: {
  network: WifiConfigNetwork;
  loading: boolean;
  error: unknown;
  onClose: () => void;
  onSave: (payload: WifiConfigUpdate) => Promise<void>;
}) {
  const [form, setForm] = useState({
    ssid: network.ssid || "",
    encryption: network.encryption || "psk2",
    key: "",
    disabled: network.disabled,
  });
  const [localError, setLocalError] = useState("");

  async function submit() {
    setLocalError("");
    const unchanged =
      form.ssid.trim() === (network.ssid || "") &&
      form.encryption === (network.encryption || "psk2") &&
      form.key === "" &&
      form.disabled === network.disabled;
    if (unchanged) {
      setLocalError("请至少修改一项配置");
      return;
    }
    try {
      const payload: WifiConfigUpdate = {
        section: network.section,
        ssid: form.ssid.trim() || undefined,
        encryption: form.encryption || undefined,
        disabled: form.disabled,
      };
      if (form.key) payload.key = form.key;
      await onSave(payload);
    } catch {
      /* mutation error surfaced via prop */
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      eyebrow="编辑无线网络"
      title={network.ssid || network.section}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={loading}>
            取消
          </Button>
          <Button onClick={submit} loading={loading}>
            确认应用
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <p className="text-sm" style={{ ...sans, color: "var(--muted)" }}>
          section <code>{network.section}</code>
          {network.device ? ` · device ${network.device}` : ""}
          {network.band ? ` · ${bandLabel(network.band)}` : ""}
        </p>
        <Field label="SSID">
          <Input
            value={form.ssid}
            onChange={(e) => setForm({ ...form, ssid: e.target.value })}
            placeholder="无线名称"
          />
        </Field>
        <Field label="加密">
          <Select
            value={form.encryption}
            onChange={(e) => setForm({ ...form, encryption: e.target.value })}
          >
            <option value="psk2">WPA2-PSK (psk2)</option>
            <option value="sae">WPA3-SAE (sae)</option>
            <option value="sae-mixed">WPA2/WPA3 mixed</option>
            <option value="psk-mixed">psk-mixed</option>
            <option value="none">开放 (none)</option>
          </Select>
        </Field>
        <Field label="密码" hint="留空则不修改">
          <Input
            type="password"
            value={form.key}
            onChange={(e) => setForm({ ...form, key: e.target.value })}
            placeholder="新密码"
            autoComplete="new-password"
          />
        </Field>
        <div className="flex items-center justify-between">
          <span className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
            禁用此网络
          </span>
          <Toggle on={form.disabled} onChange={(v: boolean) => setForm({ ...form, disabled: v })} />
        </div>
        {localError || error ? (
          <p className="text-sm" style={{ ...sans, color: "var(--danger)" }}>
            {localError || <ErrorText error={error} fallback="无线配置应用失败" showDiagnostic />}
          </p>
        ) : null}
        <p className="text-xs" style={{ ...sans, color: "var(--muted)" }}>
          应用后会执行 <code>uci commit wireless && wifi reload</code>，客户端会短暂掉线。
        </p>
      </div>
    </Modal>
  );
}

function Meta({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <p className="text-[11px]" style={{ ...sans, color: "var(--muted)" }}>
        {label}
      </p>
      <p className="mt-0.5" style={{ color: "var(--fg-strong)" }}>
        {value}
      </p>
    </div>
  );
}

function Th({ children, align = "left" }: { children: ReactNode; align?: "left" | "right" }) {
  return (
    <th
      className="px-5 py-3 text-[11px] tracking-[0.14em] uppercase font-medium"
      style={{ textAlign: align, color: "var(--muted)", ...sans }}
    >
      {children}
    </th>
  );
}

function Td({ children, align = "left" }: { children: ReactNode; align?: "left" | "right" }) {
  return (
    <td className="px-5 py-3.5 align-top" style={{ textAlign: align }}>
      {children}
    </td>
  );
}

const eyebrowMini: CSSProperties = {
  ...sans,
  fontSize: 11,
  letterSpacing: "0.16em",
  textTransform: "uppercase",
  color: "var(--muted)",
};

function bandLabel(band?: string): string {
  const b = (band || "").toLowerCase();
  if (b === "2g" || b === "2.4g" || b === "2ghz") return "2.4 GHz";
  if (b === "5g" || b === "5ghz") return "5 GHz";
  if (b === "6g" || b === "6ghz") return "6 GHz";
  return band || "未知频段";
}

function bandFromIfname(ifname?: string): string {
  if (!ifname) return "";
  if (ifname.includes("phy0") || ifname.includes("wlan0")) return "2.4G";
  if (ifname.includes("phy1") || ifname.includes("wlan1")) return "5G";
  return "";
}

function signalTone(dbm: number): Tone {
  if (!dbm) return "muted";
  if (dbm >= -55) return "ok";
  if (dbm >= -70) return "info";
  return "warn";
}

function formatSignal(dbm: number): string {
  if (!dbm) return "—";
  return `${dbm} dBm`;
}

function formatRatePair(rx?: number, tx?: number): string {
  if (!rx && !tx) return "—";
  return `↓ ${formatRate(rx)} · ↑ ${formatRate(tx)}`;
}

function formatRate(v?: number): string {
  if (!v) return "—";
  if (v >= 1000) return `${(v / 1000).toFixed(v >= 10000 ? 0 : 1)} Mbps`;
  return `${v} kbps`;
}

function formatBytes(n: number): string {
  if (!n) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 10 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}
