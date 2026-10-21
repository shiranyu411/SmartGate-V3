import { useEffect, useState, useCallback } from "react";
import type { ReactNode } from "react";
import { api } from "../lib/api";
import { ApiError } from "../lib/api";
import { setToken, subscribeToken } from "../lib/auth";
import { Button } from "../ui/Button";
import { Field, Input } from "../ui/Field";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";

type CheckData = {
  needs_setup: boolean;
  authenticated: boolean;
};

type TokenData = { token: string };

type Phase = "loading" | "setup" | "login" | "ready" | "fatal";

export function AuthGate({ children }: { children: ReactNode }) {
  const [phase, setPhase] = useState<Phase>("loading");
  const [needsSetup, setNeedsSetup] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const probe = useCallback(async () => {
    setError(null);
    setPhase("loading");
    try {
      const data = await api.get<CheckData>("/auth/check");
      setNeedsSetup(data.needs_setup);
      if (data.needs_setup) setPhase("setup");
      else if (data.authenticated) setPhase("ready");
      else setPhase("login");
    } catch (e) {
      if (e instanceof ApiError && e.code === "unauthorized") {
        setPhase("login");
        return;
      }
      setError(e);
      setPhase("fatal");
    }
  }, []);

  useEffect(() => {
    probe();
  }, [probe]);

  // token 被外部清掉（401）时，把用户带回登录
  useEffect(() => {
    return subscribeToken((t) => {
      if (!t) setPhase((p) => (p === "ready" ? "login" : p));
    });
  }, []);

  if (phase === "ready") return <>{children}</>;

  return (
    <Frame>
      {phase === "loading" ? (
        <LoadingPanel />
      ) : phase === "fatal" ? (
        <FatalPanel error={error} onRetry={probe} />
      ) : phase === "setup" ? (
        <SetupPanel
          onDone={(token) => {
            setToken(token);
            setNeedsSetup(false);
            setPhase("ready");
          }}
        />
      ) : (
        <LoginPanel
          onDone={(token) => {
            setToken(token);
            setPhase("ready");
          }}
        />
      )}
      <Footer needsSetup={needsSetup} />
    </Frame>
  );
}

function Frame({ children }: { children: ReactNode }) {
  return (
    <div
      className="min-h-screen w-full flex flex-col items-center justify-center px-6 py-16"
      style={{ background: "var(--bg)" }}
    >
      <div className="w-full max-w-[440px]">
        <header className="flex items-center gap-3 mb-10">
          <div
            className="size-9 grid place-items-center text-sm font-semibold"
            style={{
              background: "var(--accent)",
              color: "var(--accent-fg)",
              fontFamily: "var(--font-serif)",
            }}
          >
            智
          </div>
          <div className="leading-tight">
            <p
              className="font-semibold text-[17px]"
              style={{ color: "var(--fg-strong)", ...serif }}
            >
              SmartGate
            </p>
            <p style={eyebrowStyle}>书房 · 管理控制台</p>
          </div>
        </header>
        {children}
      </div>
    </div>
  );
}

function LoadingPanel() {
  return (
    <div
      className="px-8 py-10 text-center"
      style={{
        background: "var(--surface)",
        borderTop: "1px solid var(--border-strong)",
        borderBottom: "1px solid var(--border-strong)",
      }}
    >
      <p style={eyebrowStyle}>正在校验</p>
      <p
        className="mt-3 text-[20px]"
        style={{ ...serif, color: "var(--fg-strong)" }}
      >
        正在与后端打招呼…
      </p>
    </div>
  );
}

function FatalPanel({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <div
      className="px-8 py-10"
      style={{
        background: "var(--surface)",
        borderTop: "1px solid var(--border-strong)",
        borderBottom: "1px solid var(--border-strong)",
      }}
    >
      <p style={eyebrowStyle}>连接失败</p>
      <h2
        className="mt-2 text-[24px] font-medium"
        style={{ ...serif, color: "var(--fg-strong)" }}
      >
        无法连接 SmartGate
      </h2>
      <p
        className="break-words mt-3 text-sm"
        style={{ ...sans, color: "var(--muted)" }}
      >
        <ErrorText error={error} fallback="无法连接 SmartGate 后端" />
      </p>
      <p className="mt-2 text-xs" style={{ ...sans, color: "var(--muted)" }}>
        请确认后端服务已运行，开发模式下默认代理到 127.0.0.1:3000。
      </p>
      <div className="mt-6">
        <Button onClick={onRetry}>重试</Button>
      </div>
    </div>
  );
}

function SetupPanel({ onDone }: { onDone: (token: string) => void }) {
  const [pwd, setPwd] = useState("");
  const [confirm, setConfirm] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (pwd.length < 6) {
      setError("密码至少 6 位");
      return;
    }
    if (pwd !== confirm) {
      setError("两次输入不一致");
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const data = await api.post<TokenData>("/auth/setup", { password: pwd });
      onDone(data.token);
    } catch (e) {
      setError(e);
    } finally {
      setLoading(false);
    }
  }

  return (
    <form
      onSubmit={submit}
      className="px-8 py-9 space-y-6"
      style={{
        background: "var(--surface)",
        borderTop: "2px solid var(--accent)",
        borderBottom: "1px solid var(--border-strong)",
      }}
    >
      <div>
        <p style={eyebrowStyle}>首次启用</p>
        <h2
          className="mt-2 text-[26px] font-medium"
          style={{ ...serif, color: "var(--fg-strong)" }}
        >
          设置管理员密码
        </h2>
        <p className="mt-2 text-sm" style={{ ...sans, color: "var(--muted)" }}>
          该密码用于本机管理 SmartGate 网关。请妥善保管，遗失只能在路由器本地重置。
        </p>
      </div>
      <Field label="新密码" hint="至少 6 位">
        <Input
          type="password"
          autoFocus
          value={pwd}
          onChange={(e) => setPwd(e.target.value)}
          placeholder="••••••••"
        />
      </Field>
      <Field label="确认密码" error={error ? <ErrorText error={error} fallback="初始化失败" /> : undefined}>
        <Input
          type="password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          placeholder="再输入一次"
        />
      </Field>
      <div className="pt-2">
        <Button type="submit" loading={loading}>
          初始化并登录
        </Button>
      </div>
    </form>
  );
}

function LoginPanel({ onDone }: { onDone: (token: string) => void }) {
  const [pwd, setPwd] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!pwd) {
      setError("请输入密码");
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const data = await api.post<TokenData>("/auth/login", { password: pwd });
      onDone(data.token);
    } catch (e) {
      setError(e);
    } finally {
      setLoading(false);
    }
  }

  return (
    <form
      onSubmit={submit}
      className="px-8 py-9 space-y-6"
      style={{
        background: "var(--surface)",
        borderTop: "1px solid var(--border-strong)",
        borderBottom: "1px solid var(--border-strong)",
      }}
    >
      <div>
        <p style={eyebrowStyle}>登录</p>
        <h2
          className="mt-2 text-[26px] font-medium"
          style={{ ...serif, color: "var(--fg-strong)" }}
        >
          回到书房
        </h2>
        <p className="mt-2 text-sm" style={{ ...sans, color: "var(--muted)" }}>
          请输入管理员密码继续。
        </p>
      </div>
      <Field
        label="管理员密码"
        error={error ? <ErrorText error={error} context="login" fallback="登录失败" /> : undefined}
      >
        <Input
          type="password"
          autoFocus
          value={pwd}
          onChange={(e) => setPwd(e.target.value)}
          placeholder="••••••••"
        />
      </Field>
      <div className="pt-2">
        <Button type="submit" loading={loading}>
          登录
        </Button>
      </div>
    </form>
  );
}

function Footer({ needsSetup }: { needsSetup: boolean }) {
  return (
    <p
      className="mt-8 text-center text-xs"
      style={{ ...sans, color: "var(--muted)" }}
    >
      SmartGate · 书房 · {needsSetup ? "首次设置" : "管理控制台"} · 通信令牌仅保存在本机浏览器
    </p>
  );
}
