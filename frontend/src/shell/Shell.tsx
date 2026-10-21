import { useEffect, useState, useMemo } from "react";
import type { ReactNode } from "react";
import { NavLink, useLocation } from "react-router-dom";
import { Segmented } from "../ui/Segmented";
import { Button } from "../ui/Button";
import { Badge } from "../ui/Badge";
import { useOverview, useConfigState, useSetGatewayMode, useRuntimeStatus } from "../lib/queries";
import { api } from "../lib/api";
import { runtimePhaseLabel } from "../lib/statusText";
import { clearToken } from "../lib/auth";
import { DryRunModal } from "./DryRunModal";
import { PasswordModal } from "./PasswordModal";
import type { GatewayMode } from "../lib/types";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

type ThemeMode = "system" | "light" | "dark";

type NavItem = {
  to: string;
  label: string;
  hint?: string;
  badge?: string;
  disabled?: boolean;
};

const navItems: { group: string; items: NavItem[] }[] = [
  {
    group: "总览",
    items: [
      { to: "/overview", label: "概览", hint: "Overview" },
      { to: "/system", label: "系统状态", hint: "System" },
    ],
  },
  {
    group: "代理",
    items: [
      { to: "/providers", label: "订阅", hint: "Providers" },
      { to: "/nodes", label: "节点", hint: "Nodes" },
      { to: "/workers", label: "执行核心", hint: "Workers" },
      { to: "/pools", label: "业务池", hint: "Business Pools" },
      { to: "/proxy-groups", label: "选择器", hint: "Proxy Groups" },
    ],
  },
  {
    group: "网络",
    items: [
      { to: "/dns", label: "DNS", hint: "DNS" },
      { to: "/rules", label: "路由", hint: "Routes" },
      { to: "/rule-sets", label: "远程规则", hint: "Rule Sets" },
      { to: "/connections", label: "连接", hint: "Connections" },
      { to: "/wifi", label: "Wi-Fi", hint: "Wi-Fi" },
    ],
  },
  {
    group: "数据",
    items: [
      { to: "/snapshots", label: "快照", hint: "Snapshots" },
      { to: "/backups", label: "备份", hint: "Backups" },
      { to: "/scheduler", label: "计划任务", hint: "Scheduler" },
    ],
  },
];

export function Shell({
  themeMode,
  setThemeMode,
  dark,
  children,
}: {
  themeMode: ThemeMode;
  setThemeMode: (m: ThemeMode) => void;
  dark: boolean;
  children: ReactNode;
}) {
  const overview = useOverview();
  const configState = useConfigState();
  const runtime = useRuntimeStatus();
  const setMode = useSetGatewayMode();
  const [showApply, setShowApply] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const gatewayMode = (configState.data?.gateway_mode || overview.data?.gateway.mode || "safe") as GatewayMode;
  const dirty = Boolean(configState.data?.dirty ?? overview.data?.config?.dirty);
  const phase = runtime.data?.phase || (overview.data?.runtime as { phase?: string } | undefined)?.phase || "—";
  const rescue = runtime.data?.rescue_active ?? false;

  async function onChangeMode(next: GatewayMode) {
    if (next === gatewayMode) return;
    setError(null);
    try {
      await setMode.mutateAsync(next);
    } catch (e) {
      setError(e);
    }
  }

  const [sidebarOpen, setSidebarOpen] = useState(false);

  useEffect(() => {
    if (!sidebarOpen) return;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prevOverflow;
    };
  }, [sidebarOpen]);

  return (
    <div className="min-h-screen w-full lg:grid" style={{ gridTemplateColumns: "260px 1fr" }}>
      {/* Mobile overlay sidebar */}
      {sidebarOpen ? (
        <div
          className="fixed inset-0 z-40 lg:hidden"
          style={{ background: "rgba(0,0,0,0.4)" }}
          onClick={() => setSidebarOpen(false)}
        >
          <div
            className="w-[260px] h-full overflow-y-auto"
            style={{ background: "var(--surface)" }}
            onClick={(e) => e.stopPropagation()}
          >
            <Sidebar onNavigate={() => setSidebarOpen(false)} />
          </div>
        </div>
      ) : null}
      {/* Desktop sidebar */}
      <div className="hidden lg:block">
        <Sidebar />
      </div>
      <div className="min-w-0 flex flex-col">
        <TopBar
          phase={phase}
          rescue={rescue}
          gatewayMode={gatewayMode}
          gatewayLoading={setMode.isPending}
          onChangeMode={onChangeMode}
          themeMode={themeMode}
          setThemeMode={setThemeMode}
          dark={dark}
          onPasswordChange={() => setShowPassword(true)}
          onMenuToggle={() => setSidebarOpen(true)}
        />
        {dirty ? (
          <DirtyBanner
            error={error}
            onApply={() => setShowApply(true)}
          />
        ) : error ? (
          <ModeErrorBanner error={error} onDismiss={() => setError(null)} />
        ) : null}
        <main className="flex-1 min-w-0 px-4 py-6 sm:px-6 md:px-10 md:py-10">{children}</main>
      </div>
	      <DryRunModal key={showApply ? "open" : "closed"} open={showApply} onClose={() => setShowApply(false)} />
      <PasswordModal open={showPassword} onClose={() => setShowPassword(false)} />
    </div>
  );
}

function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const loc = useLocation();
  return (
    <aside
      className="sticky top-0 self-start h-screen overflow-y-auto py-8 px-6"
      style={{
        background: "var(--surface)",
        borderRight: "1px solid var(--border)",
      }}
    >
      <div className="flex items-center gap-3 mb-10">
        <div
          className="size-9 grid place-items-center text-sm font-semibold"
          style={{
            background: "var(--accent)",
            color: "var(--accent-fg)",
            ...serif,
          }}
        >
          智
        </div>
        <div className="leading-tight">
          <p className="font-semibold text-[16px]" style={{ ...serif, color: "var(--fg-strong)" }}>
            SmartGate
          </p>
          <p style={eyebrowStyle}>书房 · 管理控制台</p>
        </div>
      </div>

      <nav className="space-y-7">
        {navItems.map((g) => (
          <div key={g.group}>
            <p style={eyebrowStyle} className="mb-3">{g.group}</p>
            <ul className="space-y-0.5">
              {g.items.map((it) => (
                <li key={it.to}>
                  {it.disabled ? (
                    <DisabledNavRow item={it} />
                  ) : (
                    <NavLink
                      to={it.to}
                      className="block"
                      title={it.hint}
                      onClick={onNavigate}
                    >
                      {({ isActive }) => (
                        <NavRow item={it} active={isActive || loc.pathname === it.to} />
                      )}
                    </NavLink>
                  )}
                </li>
              ))}
            </ul>
          </div>
        ))}
      </nav>

      <p className="mt-12 text-[10px] tracking-widest" style={{ ...sans, color: "var(--muted)" }}>
        SmartGate · 书房 · 全功能版
      </p>
    </aside>
  );
}

function NavRow({ item, active }: { item: NavItem; active: boolean }) {
  return (
    <div
      className="px-3 py-2 flex items-baseline justify-between gap-2 transition-colors"
      style={{
        background: active ? "color-mix(in srgb, var(--accent) 12%, transparent)" : "transparent",
        borderLeft: active ? "2px solid var(--accent)" : "2px solid transparent",
      }}
    >
      <span
        className="text-[14px]"
        style={{
          ...sans,
          color: active ? "var(--fg-strong)" : "var(--fg)",
          fontWeight: active ? 500 : 400,
        }}
      >
        {item.label}
      </span>
      {item.hint ? (
        <span className="text-[11px]" style={{ ...sans, color: "var(--muted)", opacity: 0.7 }}>
          {item.hint}
        </span>
      ) : null}
    </div>
  );
}

function DisabledNavRow({ item }: { item: NavItem }) {
  return (
    <div
      className="px-3 py-2 flex items-baseline justify-between gap-2 cursor-not-allowed"
      style={{ opacity: 0.5 }}
      title={`${item.hint || ""} — 暂未上线`}
    >
      <span className="text-[15px]" style={{ ...serif, color: "var(--fg)" }}>
        {item.label}
      </span>
      {item.badge ? (
        <span className="text-[10px] italic" style={{ ...serif, color: "var(--muted)" }}>
          {item.badge}
        </span>
      ) : null}
    </div>
  );
}

function TopBar({
  phase,
  rescue,
  gatewayMode,
  gatewayLoading,
  onChangeMode,
  themeMode,
  setThemeMode,
  dark,
  onPasswordChange,
  onMenuToggle,
}: {
  phase: string;
  rescue: boolean;
  gatewayMode: GatewayMode;
  gatewayLoading: boolean;
  onChangeMode: (m: GatewayMode) => void;
  themeMode: ThemeMode;
  setThemeMode: (m: ThemeMode) => void;
  dark: boolean;
  onPasswordChange: () => void;
  onMenuToggle: () => void;
}) {
  const modes = useMemo(
    () =>
      [
        { key: "on", label: "开启", title: "全屋透明代理 · 日常主模式" },
        { key: "global", label: "全局", title: "全局代理 · 公网流量全走 Proxy，LAN 直连" },
        { key: "safe", label: "直连", title: "安全直连 · 代理 Worker 停止，订阅仍自动刷新" },
        { key: "off", label: "关闭", title: "完全关闭 · 代理与自动更新均停止" },
      ] as { key: GatewayMode; label: string; title: string }[],
    []
  );

  return (
    <header
      className="sticky top-0 z-30 backdrop-blur"
      style={{
        background: "color-mix(in srgb, var(--bg) 82%, transparent)",
        borderBottom: "1px solid var(--border)",
      }}
    >
      <div className="px-4 sm:px-6 md:px-10 min-h-16 py-3 flex flex-wrap items-center justify-between gap-4">
        <div className="flex items-center gap-3 min-w-0">
          <button
            className="lg:hidden size-9 grid place-items-center"
            style={{ color: "var(--fg-strong)" }}
            onClick={onMenuToggle}
            title="打开菜单"
          >
            <svg viewBox="0 0 24 24" className="size-5" fill="none" stroke="currentColor" strokeWidth="2">
              <path d="M4 6h16M4 12h16M4 18h16" strokeLinecap="round" />
            </svg>
          </button>
          <RuntimeBadge phase={phase} rescue={rescue} />
          <Segmented
            value={gatewayMode}
            onChange={(v) => onChangeMode(v as GatewayMode)}
            options={modes}
            disabled={gatewayLoading}
          />
        </div>
        <div className="flex items-center gap-3">
          <ThemeToggle mode={themeMode} setMode={setThemeMode} dark={dark} />
          <Button variant="ghost" size="sm" onClick={onPasswordChange} title="修改密码">
            改密
          </Button>
          <LogoutButton />
        </div>
      </div>
    </header>
  );
}

function RuntimeBadge({ phase, rescue }: { phase: string; rescue: boolean }) {
  const tone = rescue ? "warn" : phase === "running" ? "ok" : phase === "—" ? "muted" : "info";
  const label = rescue ? "救援模式" : phase === "—" ? phase : runtimePhaseLabel(phase);
  return (
    <Badge tone={tone}>
      <span className="size-1.5 rounded-full inline-block pulse-soft"
        style={{ background: "currentColor" }}
      />
      {label}
    </Badge>
  );
}

function ModeErrorBanner({ error, onDismiss }: { error: unknown; onDismiss: () => void }) {
  return (
    <div
      className="px-4 sm:px-6 md:px-10 py-3 flex items-start justify-between gap-3"
      style={{ background: "color-mix(in srgb, var(--danger) 12%, var(--surface))", borderBottom: "1px solid var(--border)" }}
    >
      <p className="break-words text-sm min-w-0" style={{ ...sans, color: "var(--danger)" }}>
        <ErrorText error={error} fallback="切换网关模式失败" showDiagnostic />
      </p>
      <Button variant="ghost" size="sm" onClick={onDismiss}>关闭</Button>
    </div>
  );
}

function DirtyBanner({ error, onApply }: { error: unknown; onApply: () => void }) {
  return (
    <div
      className="px-10 py-3 flex items-center justify-between gap-4"
      style={{
        background: "color-mix(in srgb, var(--warning) 14%, transparent)",
        borderBottom: "1px solid color-mix(in srgb, var(--warning) 35%, transparent)",
      }}
    >
      <div className="flex items-center gap-3 min-w-0">
        <Badge tone="warn">待应用</Badge>
        <p className="text-sm min-w-0" style={{ ...sans, color: "var(--fg)" }}>
          {error
            ? <ErrorText error={error} fallback="切换网关模式失败" showDiagnostic />
            : "你的修改已保存为期望配置，但尚未生效。请演练并确认后应用。"}
        </p>
      </div>
      <Button size="sm" onClick={onApply}>
        立即应用
      </Button>
    </div>
  );
}

function ThemeToggle({
  mode,
  setMode,
  dark,
}: {
  mode: ThemeMode;
  setMode: (m: ThemeMode) => void;
  dark: boolean;
}) {
  const items: { key: ThemeMode; label: string; icon: ReactNode }[] = [
    {
      key: "system",
      label: "跟随系统",
      icon: (
        <svg viewBox="0 0 24 24" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.8">
          <rect x="3" y="4" width="18" height="13" rx="2" />
          <path d="M8 21h8M12 17v4" strokeLinecap="round" />
        </svg>
      ),
    },
    {
      key: "light",
      label: "浅色",
      icon: (
        <svg viewBox="0 0 24 24" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.8">
          <circle cx="12" cy="12" r="4" />
          <path
            d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6l1.4 1.4M17 17l1.4 1.4M5.6 18.4L7 17M17 7l1.4-1.4"
            strokeLinecap="round"
          />
        </svg>
      ),
    },
    {
      key: "dark",
      label: "深色",
      icon: (
        <svg viewBox="0 0 24 24" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.8">
          <path d="M21 14.5A8.5 8.5 0 1 1 9.5 3a7 7 0 0 0 11.5 11.5z" />
        </svg>
      ),
    },
  ];
  return (
    <div className="flex items-center gap-2">
      <span className="hidden lg:inline text-xs" style={{ ...sans, color: "var(--muted)" }}>
        当前 · {dark ? "深色" : "浅色"}
      </span>
      <div
        className="flex items-center rounded-full p-1 gap-0.5"
        style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}
      >
        {items.map((it) => {
          const on = mode === it.key;
          return (
            <button
              key={it.key}
              title={it.label}
              onClick={() => setMode(it.key)}
              className="size-8 rounded-full grid place-items-center transition-colors"
              style={
                on
                  ? { background: "var(--accent)", color: "var(--accent-fg)" }
                  : { color: "var(--muted)" }
              }
            >
              {it.icon}
            </button>
          );
        })}
      </div>
    </div>
  );
}

function LogoutButton() {
  const [loading, setLoading] = useState(false);
  async function logout() {
    setLoading(true);
    try {
      await api.post<unknown>("/auth/logout", {});
    } catch {
      /* 401 / 网络挂掉都视为已登出 */
    } finally {
      clearToken();
      setLoading(false);
    }
  }
  return (
    <Button variant="ghost" size="sm" onClick={logout} loading={loading} title="退出登录">
      退出
    </Button>
  );
}
