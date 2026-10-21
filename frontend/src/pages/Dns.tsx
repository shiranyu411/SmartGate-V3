import { useEffect, useMemo, useState } from "react";
import { useDns, useDnsUpdate } from "../lib/queries";
import type { DnsSettings, DnsStrategy, DnsFinal } from "../lib/types";
import { Card, Eyebrow, SectionHeader, Rule } from "../ui/Card";
import { Field, Input, Select } from "../ui/Field";
import { Button } from "../ui/Button";
import { Badge } from "../ui/Badge";
import { ErrorText } from "../ui/ErrorText";
import { serif, sans, eyebrowStyle } from "../ui/tokens";

const STRATEGY_OPTIONS: { value: DnsStrategy; label: string; hint: string }[] = [
  { value: "prefer_ipv4", label: "prefer_ipv4", hint: "优先 IPv4" },
  { value: "prefer_ipv6", label: "prefer_ipv6", hint: "优先 IPv6" },
  { value: "ipv4_only", label: "ipv4_only", hint: "仅 IPv4" },
  { value: "ipv6_only", label: "ipv6_only", hint: "仅 IPv6" },
];

const FINAL_OPTIONS: { value: DnsFinal; label: string; hint: string }[] = [
  { value: "direct", label: "direct", hint: "未命中规则时本地 DNS 直连解析" },
  { value: "proxy", label: "proxy", hint: "未命中规则时走代理 DoH/DoT" },
  { value: "fallback", label: "fallback", hint: "未命中规则时走 fallback 服务器" },
];

const DEFAULT: DnsSettings = {
  strategy: "prefer_ipv4",
  classification_mode: "real_ip_nft_set",
  fakeip_enabled: false,
  fakeip_range: "",
  final: "direct",
  direct_servers: ["dhcp://auto"],
  proxy_servers: [],
  fallback_servers: [],
};

export function DnsPage() {
  const dns = useDns();
  const update = useDnsUpdate();

  const [form, setForm] = useState<DnsSettings>(DEFAULT);
  const [error, setError] = useState<unknown>(null);
  const [savedAt, setSavedAt] = useState<string | null>(null);

  useEffect(() => {
    if (dns.data) setForm(dns.data);
  }, [dns.data]);

  function patch(p: Partial<DnsSettings>) {
    setForm((f) => ({ ...f, ...p }));
    setSavedAt(null);
  }

  const validation = useMemo(() => validate(form), [form]);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    if (validation) {
      setError(validation);
      return;
    }
    try {
      await update.mutateAsync(form);
      setSavedAt(new Date().toLocaleTimeString());
    } catch (e) {
      setError(e);
    }
  }

  return (
    <div className="space-y-12 max-w-[1040px]">
      <SectionHeader
        eyebrow="DNS"
        title="域名解析策略"
        desc="保存后会进入配置 dirty，需顶栏 dry-run → apply 才会真正生效。"
        right={
          <div className="flex items-center gap-3 text-sm" style={sans}>
            {savedAt ? <Badge tone="ok">已保存于 {savedAt}</Badge> : null}
            {dns.isLoading ? <Badge tone="muted">载入中…</Badge> : null}
            {dns.error ? <Badge tone="err"><ErrorText error={dns.error} fallback="DNS 配置加载失败" /></Badge> : null}
          </div>
        }
      />

      <form onSubmit={submit} className="space-y-12">
        <Card inset>
          <div className="grid lg:grid-cols-2 gap-x-10 gap-y-6">
            <Field label="解析策略" hint={describeStrategy(form.strategy)}>
              <Select
                value={form.strategy}
                onChange={(e) => patch({ strategy: e.target.value as DnsStrategy })}
              >
                {STRATEGY_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label} · {o.hint}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="未命中规则时" hint={describeFinal(form.final)}>
              <Select
                value={form.final}
                onChange={(e) => patch({ final: e.target.value as DnsFinal })}
              >
                {FINAL_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label} · {o.hint}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
        </Card>

        <Card inset>
          <div className="grid gap-6 sm:grid-cols-2">
            <div>
              <Eyebrow>分类模式</Eyebrow>
              <p className="mt-2 text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                真实 IP + nft set
              </p>
              <p className="mt-1 text-sm" style={{ ...sans, color: "var(--muted)" }}>
                A/AAAA 应答按 Pool 写入 IPv4/IPv6 分类集合。
              </p>
            </div>
            <div className="flex items-start gap-2 sm:justify-end">
              <Badge tone="ok">真实应答</Badge>
              <Badge tone="muted">TTL 同步</Badge>
            </div>
          </div>
        </Card>

        <Rule glyph="DNS" />

        <ServerList
          label="Direct 服务器"
          required
          servers={form.direct_servers}
          onChange={(v) => patch({ direct_servers: v })}
          desc="解析国内域名与 LAN，支持 dhcp:// / udp:// / tls:// / https:// 等"
          placeholder="dhcp://auto"
        />
        <ServerList
          label="Proxy 服务器"
          required={form.final === "proxy"}
          servers={form.proxy_servers}
          onChange={(v) => patch({ proxy_servers: v })}
          desc={
            form.final === "proxy"
              ? "未命中规则时使用，必填"
              : "走代理通道解析的 DoH/DoT，未填则只有规则命中时使用"
          }
          placeholder="https://dns.google/dns-query"
        />
        <ServerList
          label="Fallback 服务器"
          required={form.final === "fallback"}
          servers={form.fallback_servers}
          onChange={(v) => patch({ fallback_servers: v })}
          desc={
            form.final === "fallback"
              ? "未命中规则时使用，必填"
              : "需要时使用的备用服务器"
          }
          placeholder="tls://8.8.8.8"
        />

        <Card inset>
          <div className="flex items-center justify-between gap-6 flex-wrap">
            <div>
              <Eyebrow>提交</Eyebrow>
              <p className="mt-2 text-sm" style={{ ...sans, color: "var(--muted)" }}>
                {validation
                  ? <span style={{ color: "var(--danger)" }}>{validation}</span>
                  : "保存后请到顶栏点 立即应用 演练并落地。"}
              </p>
              {error ? (
                <p className="break-words mt-2 text-sm" style={{ color: "var(--danger)" }}>
                  <ErrorText error={error} fallback="保存 DNS 配置失败" showDiagnostic />
                </p>
              ) : null}
            </div>
            <div className="flex items-center gap-3">
              <Button
                variant="ghost"
                onClick={(e) => {
                  e.preventDefault();
                  if (dns.data) setForm(dns.data);
                  setError(null);
                }}
                disabled={update.isPending || !dns.data}
              >
                还原
              </Button>
              <Button type="submit" loading={update.isPending}>
                保存为期望配置
              </Button>
            </div>
          </div>
        </Card>
      </form>
    </div>
  );
}

function describeStrategy(s: DnsStrategy) {
  switch (s) {
    case "prefer_ipv4":
      return "查 A 又查 AAAA，优先返回 A";
    case "prefer_ipv6":
      return "查 A 又查 AAAA，优先返回 AAAA";
    case "ipv4_only":
      return "仅查 A";
    case "ipv6_only":
      return "仅查 AAAA";
  }
}

function describeFinal(f: DnsFinal) {
  switch (f) {
    case "direct":
      return "保守：未命中规则时本地解析";
    case "proxy":
      return "积极：未命中规则时走代理 DoH";
    case "fallback":
      return "保底：未命中规则时由 fallback 决定";
  }
}

function validate(form: DnsSettings): string {
  if (form.direct_servers.length === 0) return "Direct 服务器至少需要一项";
  if (form.final === "proxy" && form.proxy_servers.length === 0)
    return "final=proxy 时，Proxy 服务器至少一项";
  if (form.final === "fallback" && form.fallback_servers.length === 0)
    return "final=fallback 时，Fallback 服务器至少一项";
  return "";
}

function ServerList({
  label,
  desc,
  servers,
  onChange,
  required,
  placeholder,
}: {
  label: string;
  desc: string;
  servers: string[];
  onChange: (v: string[]) => void;
  required?: boolean;
  placeholder?: string;
}) {
  function set(i: number, v: string) {
    const next = [...servers];
    next[i] = v;
    onChange(next);
  }
  function remove(i: number) {
    onChange(servers.filter((_, idx) => idx !== i));
  }
  function add() {
    onChange([...servers, ""]);
  }
  return (
    <Card inset>
      <div className="flex items-baseline justify-between gap-4 mb-4">
        <div>
          <p className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
            {label}
            {required ? (
              <span className="ml-2 text-xs" style={{ color: "var(--danger)" }}>
                *
              </span>
            ) : null}
          </p>
          <p className="text-xs italic mt-0.5" style={{ ...serif, color: "var(--muted)" }}>
            {desc}
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={(e) => { e.preventDefault(); add(); }}>
          添加
        </Button>
      </div>
      {servers.length === 0 ? (
        <p style={eyebrowStyle}>暂无</p>
      ) : (
        <ul className="space-y-2">
          {servers.map((s, i) => (
            <li key={i} className="flex items-center gap-2">
              <Input
                value={s}
                onChange={(e) => set(i, e.target.value)}
                placeholder={placeholder}
              />
              <Button
                variant="ghost"
                size="sm"
                onClick={(e) => {
                  e.preventDefault();
                  remove(i);
                }}
              >
                删
              </Button>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
