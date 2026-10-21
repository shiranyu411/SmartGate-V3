type ErrorContext = "default" | "login";

type ErrorTextOptions = {
  context?: ErrorContext;
  fallback?: string;
};

type ApiErrorLike = Error & {
  status: number;
  code: string;
  apiCode?: string;
};

const API_CODE_TEXT: Record<string, string> = {
  validation_failed: "提交内容校验失败，请检查后重试",
  unauthorized: "登录状态已失效，请重新登录",
  forbidden: "没有权限执行此操作",
  not_found: "请求的资源不存在",
  method_not_allowed: "当前请求方式不受支持",
  conflict: "数据已发生变化，请刷新后重试",
  locked: "操作暂时被锁定，请稍后重试",
  precondition_required: "缺少执行此操作所需的前置条件",
  rate_limited: "尝试次数过多，请稍后再试",
  upstream_failed: "上游服务暂时不可用，请稍后重试",
  insufficient_storage: "设备存储空间不足",
  internal_error: "后端服务发生内部错误",
  request_failed: "请求失败，请稍后重试",
  query_token_not_allowed: "此处不允许通过网址传递登录令牌",
  singbox_check_failed: "sing-box 配置校验失败",
  execution_graph_invalid: "配置存在阻止应用的问题",
  worker_check_failed: "执行核心配置校验失败",
  node_probe_unavailable: "节点测速未能启动，原健康状态未改变",
  node_core_unset: "请先为该节点选择执行核心",
  node_source_missing: "该节点的订阅来源已失效",
  wifi_status_failed: "无法读取无线状态",
  wifi_scan_failed: "无线网络扫描失败",
  wifi_config_read_failed: "无法读取无线网络配置",
  wifi_config_apply_failed: "无线网络配置应用失败",
  worker_not_found: "未找到指定的 Worker",
};

const ERROR_KIND_TEXT: Record<string, string> = {
  unauthorized: API_CODE_TEXT.unauthorized,
  locked: API_CODE_TEXT.locked,
  conflict: API_CODE_TEXT.conflict,
  precondition: API_CODE_TEXT.precondition_required,
  bad_request: API_CODE_TEXT.validation_failed,
  insufficient_storage: API_CODE_TEXT.insufficient_storage,
  server: API_CODE_TEXT.internal_error,
  network: "无法连接 SmartGate 后端",
  unknown: API_CODE_TEXT.request_failed,
};

function isApiError(error: unknown): error is ApiErrorLike {
  if (!(error instanceof Error)) return false;
  const candidate = error as Partial<ApiErrorLike>;
  return typeof candidate.status === "number" && typeof candidate.code === "string";
}

function hasChinese(text: string): boolean {
  return /[\u3400-\u9fff]/u.test(text);
}

export function errorText(error: unknown, options: ErrorTextOptions = {}): string {
  const fallback = options.fallback ?? "操作失败，请稍后重试";

  if (isApiError(error)) {
    if (error.apiCode === "rate_limited") return API_CODE_TEXT.rate_limited;
    if (
      options.context === "login" &&
      (error.apiCode === "unauthorized" || error.code === "unauthorized")
    ) {
      return "密码错误";
    }
    if (error.message.trim() && hasChinese(error.message)) return error.message;
    if (error.apiCode && API_CODE_TEXT[error.apiCode]) return API_CODE_TEXT[error.apiCode];
    return ERROR_KIND_TEXT[error.code] ?? fallback;
  }

  if (typeof error === "string" && error.trim() && hasChinese(error)) {
    return error;
  }
  if (error instanceof Error && error.message.trim() && hasChinese(error.message)) {
    return error.message;
  }
  return fallback;
}

export function errorDiagnostic(error: unknown): string | undefined {
  if (!(error instanceof Error) || !error.message.trim()) return undefined;
  if (!isApiError(error)) return error.message;
  const code = error.apiCode || error.code;
  return code ? `[${code}] ${error.message}` : error.message;
}
