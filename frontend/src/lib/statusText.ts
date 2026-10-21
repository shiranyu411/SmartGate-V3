const CUTOVER_STAGE_TEXT: Record<string, string> = {
  requested: "准备执行路径",
  preparing: "准备执行路径",
  resolve_target: "解析目标路径",
  start_target_worker: "启动目标 Worker",
  select_target_executor: "选择目标执行器",
  target_preflight: "目标路径预检",
  closing: "关闭旧连接",
  prepare_fabric: "准备网关转发",
  fabric_switch: "切换网关转发",
  resolve_source: "解析旧路径",
  close_old_binding: "关闭旧连接",
  verifying: "出口验证",
  verify_new_path: "出口验证",
  commit_target: "提交目标路径",
  start_drain: "启动连接排空",
  draining: "排空中",
  retrying: "重试清理中",
  failed: "清理失败",
  drain_failed: "清理失败",
  drain_recovered: "清理已恢复",
  drain_complete: "排空已完成",
  cutover_complete: "切换已完成",
  residual_connections: "存在残留连接",
  binding_superseded: "已被后续切换取代",
  invalid_cutover_result: "切换结果异常",
  selection_conflict: "选择已发生变化",
  cutover_conflict: "切换操作冲突",
  requires_config_apply: "需先应用配置",
  cutover_failed: "切换失败",
  request_failed: "切换请求失败",
  persist_committed: "保存切换结果",
  persist_drain: "保存排空状态",
  remove_committed_journal: "清理事务记录",
  stop_unbound_worker: "停止旧 Worker",
};

const CUTOVER_PHASE_TEXT: Record<string, string> = {
  preparing: "准备执行路径",
  closing: "关闭旧连接",
  verifying: "出口验证",
  draining: "排空中",
  retrying: "重试清理中",
  drain_failed: "清理失败",
  complete: "切换已完成",
  partial: "切换部分完成",
  failed: "切换失败",
  conflict: "切换操作冲突",
  requires_apply: "需先应用配置",
  superseded: "已被后续切换取代",
};

const RUNTIME_PHASE_TEXT: Record<string, string> = {
  starting: "正在启动",
  running: "运行中",
  rescue: "救援模式",
  stopped: "已停止",
};

const SERVICE_STATUS_TEXT: Record<string, string> = {
  running: "运行中",
  stub: "未接入",
  not_running: "已停止或异常",
  stopped: "已停止",
  failed: "异常",
};

const SNAPSHOT_KIND_TEXT: Record<string, string> = {
  manual: "手动",
  scheduled: "定时",
  "before-edit": "编辑前",
  before_edit: "编辑前",
  automatic: "自动",
  auto: "自动",
  "pre-apply": "应用前",
  pre_apply: "应用前",
  config_apply: "配置应用前",
  provider_apply: "订阅更新前",
  snapshot_rollback: "快照回滚前",
  backup_restore: "备份恢复前",
};

const RESULT_STATUS_TEXT: Record<string, string> = {
  applied: "已应用",
  restored: "已恢复",
  ok: "已完成",
};

const ROLLBACK_STAGE_TEXT: Record<string, string> = {
  transaction_apply: "应用事务",
  singbox_apply: "应用 sing-box 配置",
  openwrt_apply: "应用 OpenWrt 配置",
  resolve_target: "解析目标路径",
  start_target_worker: "启动目标 Worker",
  select_target_executor: "选择目标执行器",
  target_preflight: "目标路径预检",
  prepare_fabric: "准备网关转发",
  fabric_switch: "切换网关转发",
  resolve_source: "解析旧路径",
  close_old_binding: "关闭旧连接",
  residual_connections: "清理残留连接",
  verify_new_path: "验证新出口",
  start_drain: "启动旧连接排空",
  persist_committed: "保存切换结果",
  persist_drain: "保存排空状态",
  remove_committed_journal: "清理事务记录",
  stop_unbound_worker: "停止旧 Worker",
  cutover_transaction: "切换事务",
};

const CAPABILITY_STATUS_TEXT: Record<string, string> = {
  supported: "支持",
  degraded: "降级",
  unsupported_by_core: "核心不支持",
};

const WORKER_EVENT_KIND_TEXT: Record<string, string> = {
  start: "开始启动",
  install_failed: "安装失败",
  route_prepare_failed: "路由准备失败",
  ready: "启动就绪",
  configured: "配置已安装",
  drain: "开始排空",
  drain_complete: "排空完成",
  stop_failed: "停止失败",
  stopped: "已停止",
  restore_failed: "恢复失败",
  restored: "已恢复",
  rescue_stop: "救援模式停止",
  rescue_stop_failed: "救援模式停止失败",
  cutover_start: "切换路径启动",
  cutover_start_failed: "切换路径启动失败",
  cutover_ready: "切换路径就绪",
  exited: "进程异常退出",
};

const FAILOVER_KIND_TEXT: Record<string, string> = {
  switch: "切换节点",
  fail_closed: "故障关闭",
  recovery: "恢复连接",
  noop: "无需切换",
  manual: "手动选点",
  restore: "恢复选点",
  sync: "同步选点",
};

const FAILOVER_REASON_TEXT: Record<string, string> = {
  no_healthy_candidate_block: "无健康候选，按策略阻断",
  no_healthy_candidate_hold: "无健康候选，保持当前路径",
  no_healthy_candidate_direct: "无健康候选，切换为直连",
  select_first_healthy: "选择首个健康候选",
  healthy_candidate_recovered: "健康候选已恢复",
  current_is_healthy: "当前节点健康",
  current_unhealthy_switch_next: "当前节点异常，切换下一候选",
  manual_select: "手动选点",
  manual_revert: "手动恢复上一节点",
  auto_failover_disabled: "自动故障切换未启用",
  probe_incomplete_skip: "健康探测尚未完成",
  failover_locked: "业务池已锁定",
  cooldown_active: "仍在切换冷却期",
  no_health_sample_skip: "暂无健康样本",
  current_failure_unconfirmed: "当前节点故障尚未确认",
  candidate_failures_unconfirmed: "候选节点故障尚未确认",
  worker_failed_auto_failover_disabled: "Worker 故障，但自动切换未启用",
  worker_failed_switch_candidate: "Worker 故障，切换健康候选",
  worker_failed_no_candidate_block: "Worker 故障且无候选，按策略阻断",
  worker_failed_no_candidate_direct: "Worker 故障且无候选，切换为直连",
  worker_failed_no_candidate_hold: "Worker 故障且无候选，保持当前路径",
  worker_failed_invalid_no_candidate_policy: "Worker 故障切换策略无效",
  config_apply_restore_proxy_selection: "应用配置后恢复选点",
  config_apply_sync_selector_state: "应用配置后同步选点",
  cutover_compensation_failed: "切换补偿失败",
  no_healthy_sensitive_pool_candidate: "敏感业务池没有健康候选",
};

const FAILOVER_STATUS_TEXT: Record<string, string> = {
  applied: "已应用",
  failed: "失败",
  skipped: "已跳过",
  held: "保持当前",
};

const CUTOVER_RESULT_TEXT: Record<string, string> = {
  cutover_complete: "切换完成",
  cutover_partial: "部分完成",
  committed: "已提交",
  skipped: "已跳过",
};

const FAILOVER_SOURCE_TEXT: Record<string, string> = {
  manual: "手动",
  watchdog: "自动巡检",
  worker_supervisor: "Worker 监督",
};

const DRAIN_STATUS_TEXT: Record<string, string> = {
  draining: "平滑排空中",
  retrying: "重试清理中",
  failed: "排空失败",
};

export function drainStatusLabel(status: string): string {
  return DRAIN_STATUS_TEXT[status] ?? "排空处理中";
}

/* 节点/候选健康状态（NodeHealthStatus + ProxyGroupCandidate.health） */
const HEALTH_STATUS_TEXT: Record<string, string> = {
  ok: "正常",
  failed: "失败",
  degraded: "降级",
};

export function healthStatusLabel(status?: string): string {
  if (!status) return "未测";
  return HEALTH_STATUS_TEXT[status] ?? "未测";
}

/* 配置应用结果里 workers[].status / core_results[].status（后端取值 running/applied） */
const APPLY_STATUS_TEXT: Record<string, string> = {
  running: "运行中",
  applied: "已应用",
  ok: "已完成",
  failed: "失败",
};

export function applyStatusLabel(status: string): string {
  return APPLY_STATUS_TEXT[status] ?? "状态未知";
}

/* 路由解释 RouteExplain.decision（internal/routes/explain.go 的 Decision 枚举） */
const ROUTE_DECISION_TEXT: Record<string, string> = {
  LAN_Bypass: "局域网旁路",
  AI: "AI 服务",
  Banking: "金融",
  Crypto: "加密货币",
  Streaming: "流媒体",
  Direct: "直连",
  GeneralProxy: "通用代理",
  Block: "阻断",
  Geo: "地理规则",
  Pool: "业务池",
};

export function routeDecisionLabel(decision?: string): string {
  if (!decision) return "—";
  return ROUTE_DECISION_TEXT[decision] ?? decision;
}

/* 路由解释 RouteExplain.reason（explain.go 中的枚举串） */
const ROUTE_REASON_TEXT: Record<string, string> = {
  empty_target: "目标为空，按策略阻断",
  private_or_loopback_ip: "内网或回环地址，走局域网旁路",
  public_ip_default_proxy: "公网 IP，未命中规则，走默认代理",
  default_proxy: "未命中规则，走默认代理",
  custom_ip_rule: "命中自定义 IP 规则",
  custom_domain_rule: "命中自定义域名规则",
  geo_domain_rule: "命中地理域名规则",
  geo_ip_rule: "命中地理 IP 规则",
};

export function routeReasonLabel(reason?: string): string {
  if (!reason) return "—";
  return ROUTE_REASON_TEXT[reason] ?? reason;
}

const SERVICE_ACTION_TEXT: Record<string, string> = {
  restart: "重启",
  reload: "重载",
};

export function serviceActionLabel(action?: string): string {
  if (!action) return "操作";
  return SERVICE_ACTION_TEXT[action] ?? action;
}

const RULE_FORMAT_TEXT: Record<string, string> = {
  binary: "二进制",
  source: "源文本",
};

export function ruleFormatLabel(format?: string): string {
  if (!format) return "二进制";
  return RULE_FORMAT_TEXT[format] ?? format;
}

/* 救援模式原因（后端取值 manual / data_plane_unavailable） */
const RESCUE_REASON_TEXT: Record<string, string> = {
  manual: "手动进入",
  data_plane_unavailable: "数据面不可用",
};

export function rescueReasonLabel(reason?: string): string {
  if (!reason) return "手动进入";
  return RESCUE_REASON_TEXT[reason] ?? reason;
}

/* 系统日志读取结果（后端仅在成功时返回 "ok"，失败走错误信封） */
export function systemLogsResultLabel(result?: string): string {
  if (!result) return "—";
  return result === "ok" ? "读取成功" : "状态未知";
}

/* dry-run 变更分类：后端基本返回中文，唯一的英文值是 rule-conflict */
export function dryRunCategoryLabel(category: string): string {
  return category === "rule-conflict" ? "规则冲突" : category;
}

export function cutoverStageLabel(stage?: string, phase?: string): string {
  if (stage && CUTOVER_STAGE_TEXT[stage]) return CUTOVER_STAGE_TEXT[stage];
  if (phase && CUTOVER_PHASE_TEXT[phase]) return CUTOVER_PHASE_TEXT[phase];
  return "切换处理中";
}

export function drainErrorLabel(error: string): string {
  if (/[\u3400-\u9fff]/u.test(error)) return error;
  const normalized = error.toLowerCase();
  if (normalized.includes("persist drain")) return "排空状态写盘失败";
  if (normalized.includes("worker stop") || normalized.includes("stop unbound worker")) {
    return "停止旧 Worker 失败";
  }
  if (normalized.includes("close")) return "关闭旧连接失败";
  if (normalized.includes("deadline exceeded")) return "等待旧连接排空超时";
  return "清理旧连接失败";
}

export function runtimePhaseLabel(phase: string): string {
  return RUNTIME_PHASE_TEXT[phase] ?? "未知阶段";
}

export function serviceStatusLabel(status: string): string {
  return SERVICE_STATUS_TEXT[status] ?? "状态异常";
}

export function snapshotKindLabel(kind: string): string {
  return SNAPSHOT_KIND_TEXT[kind] ?? "其他类型";
}

export function resultStatusLabel(status: string): string {
  return RESULT_STATUS_TEXT[status] ?? "已完成";
}

export function snapshotResultStatusLabel(status: string): string {
  return status === "ok" || status === "restored" ? "已恢复" : resultStatusLabel(status);
}

export function rollbackStageLabel(stage: string): string {
  return ROLLBACK_STAGE_TEXT[stage] ?? "回滚处理";
}

export function capabilityStatusLabel(status: string): string {
  return CAPABILITY_STATUS_TEXT[status] ?? "状态未知";
}

export function workerEventKindLabel(kind: string): string {
  return WORKER_EVENT_KIND_TEXT[kind] ?? "状态更新";
}

export function failoverKindLabel(kind?: string): string {
  return kind ? FAILOVER_KIND_TEXT[kind] ?? "状态更新" : "—";
}

export function failoverReasonLabel(reason?: string): string {
  return reason ? FAILOVER_REASON_TEXT[reason] ?? "其他原因" : "—";
}

export function failoverStatusLabel(status?: string): string {
  return status ? FAILOVER_STATUS_TEXT[status] ?? "状态未知" : "—";
}

export function cutoverResultLabel(result?: string): string {
  return result ? CUTOVER_RESULT_TEXT[result] ?? "切换结果未知" : "—";
}

export function failoverSourceLabel(source?: string): string {
  return source ? FAILOVER_SOURCE_TEXT[source] ?? "其他来源" : "—";
}
