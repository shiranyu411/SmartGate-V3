// SmartGate API 客户端：信封解包、Bearer 注入、错误分类
// 生产环境同源部署，dev 由 vite 代理转发到后端

import { getToken, clearToken } from "./auth";

export const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) || "/api";

// 后端契约：除 /healthz 和 /api/runtime/locks 外，所有响应都是 envelope
export type StructuredApiError = {
  code: string;
  message: string;
  details?: unknown;
};

export type Envelope<T> =
  | { ok: true; data: T }
  | { ok: false; error: StructuredApiError };

export class ApiError extends Error {
  status: number;
  code: "unauthorized" | "locked" | "conflict" | "precondition" | "bad_request" | "insufficient_storage" | "server" | "network" | "unknown";
  apiCode?: string;
  details?: unknown;
  payload: unknown;

  constructor(
    message: string,
    status: number,
    code: ApiError["code"],
    payload?: unknown,
    apiCode?: string,
    details?: unknown,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.payload = payload;
    this.apiCode = apiCode;
    this.details = details;
  }
}

function classify(status: number): ApiError["code"] {
  if (status === 401) return "unauthorized";
  if (status === 423) return "locked";
  if (status === 409) return "conflict";
  if (status === 428) return "precondition";
  if (status === 400 || status === 422) return "bad_request";
  if (status === 507) return "insufficient_storage";
  if (status >= 500) return "server";
  return "unknown";
}

function structuredError(value: unknown): StructuredApiError | undefined {
  if (!value || typeof value !== "object") return undefined;
  const candidate = value as Partial<StructuredApiError>;
  if (typeof candidate.code !== "string" || typeof candidate.message !== "string") return undefined;
  return { code: candidate.code, message: candidate.message, details: candidate.details };
}

function envelope<T>(value: unknown): Envelope<T> | undefined {
  if (!value || typeof value !== "object") return undefined;
  const candidate = value as { ok?: unknown; data?: unknown; error?: unknown };
  if (candidate.ok === true && "data" in candidate) {
    return { ok: true, data: candidate.data as T };
  }
  if (candidate.ok === false) {
    const error = structuredError(candidate.error);
    if (error) return { ok: false, error };
  }
  return undefined;
}

type RequestOpts = {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
  // 关掉信封解包，给少数返回纯文本的端点用 (/healthz, /api/runtime/locks)
  raw?: boolean;
  headers?: Record<string, string>;
};

// 完整 URL：path 以 / 开头时拼接 API_BASE 之前的来源；否则视为 API_BASE 内的路径
function buildUrl(path: string): string {
  if (path.startsWith("http://") || path.startsWith("https://")) return path;
  if (path === "/healthz") return path;
  // path 可能是 "/api/xxx" 或 "/xxx"；统一转换
  if (path.startsWith("/api/")) return path.replace(/^\/api/, API_BASE);
  if (path.startsWith("/")) return `${API_BASE}${path}`;
  return `${API_BASE}/${path}`;
}

export async function request<T>(path: string, opts: RequestOpts = {}): Promise<T> {
  const url = buildUrl(path);
  const headers: Record<string, string> = {
    Accept: "application/json",
    ...(opts.headers || {}),
  };
  if (opts.body !== undefined && !(opts.body instanceof FormData)) {
    headers["Content-Type"] = "application/json";
  }
  const token = getToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;

  let resp: Response;
  try {
    resp = await fetch(url, {
      method: opts.method || (opts.body !== undefined ? "POST" : "GET"),
      headers,
      body:
        opts.body === undefined
          ? undefined
          : opts.body instanceof FormData
          ? opts.body
          : JSON.stringify(opts.body),
      signal: opts.signal,
    });
  } catch (e) {
    const msg = e instanceof Error ? e.message : "network error";
    throw new ApiError(`无法连接后端：${msg}`, 0, "network");
  }

  if (opts.raw) {
    if (!resp.ok) {
      if (resp.status === 401) clearToken();
      throw new ApiError(`HTTP ${resp.status}`, resp.status, classify(resp.status));
    }
    return (await resp.text()) as unknown as T;
  }

  // 非 JSON 响应（异常）
  const ctype = resp.headers.get("Content-Type") || "";
  if (!ctype.includes("application/json")) {
    const text = await resp.text().catch(() => "");
    if (!resp.ok) {
      if (resp.status === 401) clearToken();
      throw new ApiError(text || `HTTP ${resp.status}`, resp.status, classify(resp.status));
    }
    // 文本 200，按 raw 处理给上层
    return text as unknown as T;
  }

  const payload = await resp.json();
  const env = envelope<T>(payload);
  if (!env) {
    if (resp.status === 401) clearToken();
    throw new ApiError("后端返回了无效的 API 响应", resp.status, classify(resp.status), payload);
  }
  if (!resp.ok || env.ok === false) {
    const apiError = env.ok === false ? structuredError(env.error) : undefined;
    let msg = apiError?.message ?? `HTTP ${resp.status}`;
    // Guide operators when a residual journal blocks mutations (B5 / C1).
    if (
      resp.status === 423 &&
      typeof msg === "string" &&
      msg.includes("durable transaction recovery is required") &&
      !msg.includes("POST /api/runtime/recover")
    ) {
      msg = `${msg}；存在未完成事务，可调用 POST /api/runtime/recover 恢复`;
    }
    const error = new ApiError(
      msg || `HTTP ${resp.status}`,
      resp.status,
      classify(resp.status),
      env,
      apiError?.code,
      apiError?.details,
    );
    // Parse the structured error before invalidating auth so callers retain its message/details.
    if (resp.status === 401) clearToken();
    throw error;
  }
  return env.data;
}

export const api = {
  get: <T>(path: string, opts?: RequestOpts) => request<T>(path, { ...opts, method: "GET" }),
  post: <T>(path: string, body?: unknown, opts?: RequestOpts) =>
    request<T>(path, { ...opts, method: "POST", body }),
  put: <T>(path: string, body?: unknown, opts?: RequestOpts) =>
    request<T>(path, { ...opts, method: "PUT", body }),
  del: <T>(path: string, body?: unknown, opts?: RequestOpts) =>
    request<T>(path, { ...opts, method: "DELETE", body }),
};

// 生成 op_id：契约要求每次用户动作唯一稳定
export function makeOpId(prefix = "ui"): string {
  const suffix = Math.random().toString(36).slice(2, 8);
  return `${prefix}-${Date.now()}-${suffix}`;
}
