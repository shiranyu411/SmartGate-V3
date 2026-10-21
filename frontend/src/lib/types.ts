// 与 FRONTEND_API_CONTRACT.md 对齐的共享类型
// Phase 1/2/3 用到的字段。后续按需扩展

export type GatewayMode = "safe" | "global" | "on" | "off";
export type RuntimePhase = string;

export type RuntimeStatus = {
  phase: RuntimePhase;
  listen_addr: string;
  config_epoch: number;
  topology_hash: string;
  gateway_mode?: GatewayMode;
  rescue_active: boolean;
  rescue_reason: string;
  workers: WorkerStatus[];
  bindings: PoolBinding[];
  cutovers_in_flight: CutoverProgress[];
  drains: DrainProgress[];
};

export type ConfigState = {
  dirty: boolean;
  spec_hash: string;
  topology_hash: string;
  config_epoch: number;
  gateway_mode: GatewayMode;
  saved_gateway_mode: GatewayMode;
  applied_gateway_mode?: GatewayMode;
  running_gateway_mode?: GatewayMode;
};

export type Overview = {
  runtime: RuntimeStatus | Record<string, never>;
  gateway: {
    mode: GatewayMode;
    saved_mode: GatewayMode;
    applied_mode?: GatewayMode;
    running_mode?: GatewayMode;
    dirty: boolean;
  };
  config: ConfigState;
  providers: { total: number; enabled: number };
  nodes: { total: number; usable: number };
  business_pools: { total: number };
  pending?: {
    dirty: boolean;
    providers: { total: number; enabled: number };
    nodes: { total: number; usable: number };
    business_pools: { total: number };
  };
  snapshots: { pending: number };
};

export type DryRunResult = {
  id: string;
  created_at: string;
  spec_hash: string;
  topology_hash: string;
  config_epoch: number;
  gateway_mode: GatewayMode;
  runtime_action?: string;
  changes?: Array<{
    category: string;
    title: string;
    before?: string;
    after?: string;
    impact: string;
    blocking?: boolean;
  }>;
  changes_complete?: boolean;
  changes_omitted?: number;
};

export type ApplyResult = {
  status: string;
  config_epoch: number;
  topology_hash: string;
  workers: Array<{
    id: string;
    status: string;
  }>;
  core_results: Array<{
    core: CoreID;
    status: string;
    workers?: string[];
  }>;
  fabric_applied: boolean;
  openwrt?: unknown;
  openwrt_applied?: boolean;
  runtime_action?: string;
  resource_guard?: {
    enabled: boolean;
    overlay_path: string;
    free_bytes: number;
    candidate_bytes: number;
    soft_limit_bytes: number;
    hard_limit_bytes: number;
    warnings: string[];
  };
  warnings?: string[];
};

export type ApplyRollbackReport = {
  attempted: boolean;
  ok: boolean;
  stage?: string;
  reason?: string;
  workers_restored: boolean;
  fabric_restored: boolean;
  bindings_restored: boolean;
  selection_restored: boolean;
  errors?: string[];
};

export type ApplyErrorPayload = {
  ok: false;
  error: {
    code: string;
    message: string;
    details?: {
      rollback?: ApplyRollbackReport;
    };
  };
};

export type VersionInfo = {
  name: string;
  version: string;
  commit: string;
  built_at: string;
  go_version: string;
  goos: string;
  goarch: string;
};

// ===== Providers =====

export type ProviderType = "subscription" | "manual" | "local-file" | "temporary";
export type ProviderFetchMode = "direct" | "via_bootstrap";

export type ProviderSubscriptionInfo = {
  upload_bytes: number;
  download_bytes: number;
  used_bytes: number;
  total_bytes: number;
  remaining_bytes: number;
  expire_unix?: number;
  expire_at?: string;
  updated_at: string;
  raw?: string;
};

export type Provider = {
  id: string;
  name: string;
  type: ProviderType;
  enabled: boolean;
  fetch_mode: ProviderFetchMode;
  roles: string[];
  bootstrap: string;
  url_configured: boolean;
  url_masked?: string;
  file_path_configured: boolean;
  user_agent?: string;
  priority: number;
  include_keywords: string[];
  exclude_keywords: string[];
  node_count?: number;
  last_refresh_at?: string;
  subscription?: ProviderSubscriptionInfo;
};

export type ProviderInput = Pick<
  Provider,
  | "id"
  | "name"
  | "type"
  | "enabled"
  | "fetch_mode"
  | "roles"
  | "bootstrap"
  | "priority"
  | "include_keywords"
  | "exclude_keywords"
> & {
  url?: string;
  file_path?: string;
  user_agent?: string;
};

export type ProviderChange =
  | { action: "upsert"; provider: ProviderInput }
  | { action: "delete"; id: string };

export type ProviderRefreshStatus = "ok" | "unchanged" | "stale" | "failed";

export type ProviderRefreshLog = {
  time: string;
  scope: string;
  provider_id: string;
  status: ProviderRefreshStatus;
  changed: boolean;
  count?: number;
  error?: string;
};

export type ProviderRefreshResult = {
  plan: {
    steps: Array<{ provider_id: string; fetch_mode: ProviderFetchMode }>;
  };
  results: Array<{
    provider_id: string;
    status: ProviderRefreshStatus;
    changed: boolean;
    metadata_only?: boolean;
    count?: number;
    subscription?: ProviderSubscriptionInfo;
    subscription_error?: string;
    error?: string;
  }>;
  changed: boolean;
};

// ===== Nodes =====

export type CoreID = string;
export type CoreInfo = {
  id: CoreID;
  display_name: string;
};
export type CapabilityStatus = "supported" | "unsupported_by_core" | "degraded";

export type CoreCapability = {
  status: CapabilityStatus;
  reasons: string[];
  reason_texts?: string[];
};

export type CapabilityMatrix = Record<CoreID, CoreCapability | undefined>;

export type SmartNodeTransport = {
  type: string;
  path?: string;
  host?: string;
  service_name?: string;
};

export type SmartNodeSecurity = {
  type: string;
  sni?: string;
  alpn?: string[];
  fingerprint?: string;
  flow?: string;
  public_key_masked?: string;
  short_id?: string;
  skip_cert_verify?: boolean;
  certificate_masked?: string;
};

export type SmartNodeIdentity = {
  protocol: string;
  server: string;
  port: number;
  transport: SmartNodeTransport;
  security: SmartNodeSecurity;
  secret_ref_masked: string;
};

export type SmartNodeAlias = {
  provider_id: string;
  raw_tag: string;
  first_seen: string;
  last_seen: string;
};

export type NodeHealthStatus = "ok" | "failed";

export type NodeHealthContext = {
  revision: number;
  core?: CoreID;
  worker?: string;
  method: string;
};

export type NodeHealth = {
  last_test?: string;
  status: NodeHealthStatus;
  delay_ms?: number;
  test_url?: string;
  updated_at?: string;
  error?: string;
  consecutive_failures?: number;
  context: NodeHealthContext;
};

export type SmartNode = {
  id: string;
  revision: number;
  display_name: string;
  identity: SmartNodeIdentity;
  aliases: SmartNodeAlias[];
  capability: CapabilityMatrix;
  core_choice: CoreID | null;
  referenced_by_pools: string[];
  source_missing?: boolean;
  health?: NodeHealth;
};

export type CoreChoiceResult = {
  smart_node: SmartNode;
  affected_pools: string[];
  dirty: boolean;
  metadata_only?: boolean;
};

// ===== Workers & Bindings =====

export type WorkerState = "stopped" | "starting" | "running" | "draining" | "failed";

export type NodeExecutor = {
  id: string;
  smart_node: string;
  revision: number;
  core: CoreID;
  worker: string;
};

export type WorkerEvent = {
  at: string;
  worker: string;
  kind: string;
  state: WorkerState | string;
  message?: string;
  diagnostic?: string;
};

export type WorkerStatus = {
  id: string;
  core: CoreID;
  version?: string;
  state: WorkerState;
  executors: NodeExecutor[];
  pools: string[];
  stats?: {
    rss_kb?: number;
    fd?: number;
    connections?: number;
  };
  last_event?: WorkerEvent;
};

export type PoolBinding = {
  id: string;
  pool: string;
  target: {
    kind: string;
    smart_node?: string;
    executor?: string;
    core?: CoreID;
    worker?: string;
  };
  generation: number;
  since: string;
};

export type CutoverProgress = {
  pool: string;
  stage: string;
};

export type DrainProgress = {
  pool: string;
  old_generation: number;
  remaining: number;
  deadline?: string;
  status: "draining" | "retrying" | "failed";
  last_error?: string;
  retry_count: number;
};

// ===== Business Pools =====

export type PoolRole =
  | "general"
  | "streaming"
  | "ai"
  | "banking"
  | "crypto"
  | "direct"
  | "block";

export type CutoverPolicy = "strict" | "graceful";
export type NoCandidatePolicy = "block" | "direct" | "hold";

export type PoolPolicy = {
  cutover: CutoverPolicy;
  graceful_drain_seconds: number;
  no_candidate: NoCandidatePolicy;
  auto_failover: boolean;
};

export type BusinessPool = {
  id: string;
  name: string;
  role: PoolRole;
  node_ids: string[];
  policy: PoolPolicy;
  builtin_origin?: string;
  members_core_summary?: Partial<Record<CoreID | "unset", number>>;
};

export type BusinessPoolChange =
  | { action: "upsert"; pool: BusinessPool }
  | { action: "delete"; id: string };

// ===== Proxy Groups & Failover =====

export type ProxyGroup = {
  pool: string;
  binding: PoolBinding;
  candidates: ProxyGroupCandidate[];
  lock: { reason?: string; locked_at: string } | null;
  cooldown_until: string | null;
  failover_enabled: boolean;
};

export type ProxyGroupCandidate = {
  smart_node: string;
  core?: CoreID;
  health: string;
  delay_ms?: number;
};

export type CutoverSource = {
  smart_node: string;
  generation: number;
};

export type CutoverDrain = {
  old_generation: number;
  remaining: number;
  deadline: string;
};

export type CutoverResult = {
  result: "cutover_complete" | "cutover_partial" | "committed" | string;
  binding: PoolBinding;
  closed_connections?: number;
  residual_connections?: number;
  verified: boolean;
  draining?: CutoverDrain;
};

export type SelectionConflictDetails = {
  pool: string;
  expected?: CutoverSource;
  actual?: CutoverSource;
  reason?: string;
};

export type RequiresConfigApplyDetails = {
  pool: string;
  target: string;
  reason?: string;
};

export type CutoverFailureDetails = {
  pool?: string;
  stage?: string;
  rolled_back?: boolean;
  blocked?: boolean;
  reason?: string;
};

export type FailoverEvent = {
  time: string;
  pool_id: string;
  role: string;
  from: string;
  to: string;
  kind: string;
  reason: string;
  generation_from?: number;
  generation_to?: number;
  executor?: string;
  cutover_result?: string;
  source?: string;
  status?: string;
  error?: string;
};

export type FailoverDecision = FailoverEvent;

export type FailoverRunResult = {
  decisions: FailoverDecision[];
  applied: FailoverDecision[];
  events: FailoverEvent[];
  failures?: Array<{ pool_id: string; error: string }>;
};

export type FailoverPoolState = {
  pool_id: string;
  role: string;
  current: string;
  previous: string;
  last_kind: string;
  last_reason: string;
  last_changed_at: string;
  cooldown_until?: string;
  terminal?: boolean;
};

export type FailoverState = {
  enabled: boolean;
  cooldown_seconds: number;
  locks: Record<string, { reason: string; locked_at: string }>;
  pools: Record<string, FailoverPoolState>;
};

// ===== DNS =====

export type DnsStrategy = "prefer_ipv4" | "prefer_ipv6" | "ipv4_only" | "ipv6_only";
export type DnsFinal = "direct" | "proxy" | "fallback";

export type DnsSettings = {
  strategy: DnsStrategy;
  classification_mode: "real_ip_nft_set";
  fakeip_enabled: boolean;
  fakeip_range: string;
  final: DnsFinal;
  direct_servers: string[];
  proxy_servers: string[];
  fallback_servers: string[];
};

// ===== Routes =====

export type RuleSource = "builtin-v2" | "geo-default" | "user";
export type RuleKind =
  | "domain_suffix"
  | "domain_keyword"
  | "ip_cidr"
  | "geosite"
  | "geoip";

export type RouteRule = {
  id: string;
  source: RuleSource;
  kind: RuleKind;
  value: string;
  pool_id: string;
  enabled: boolean;
  overridden_by?: string;
};

export type RuleConflict = {
  rule_key: string;
  kind: RuleKind;
  value: string;
  pools: string[];
  rule_ids: string[];
};

export type RulesData = {
  rules: RouteRule[];
  conflicts: RuleConflict[];
};

export type RemoteRuleTarget = "direct" | "proxy" | "block";
export type RemoteRuleKind = "geo" | "adblock" | "custom" | "provider";
export type RemoteRuleFormat = "binary" | "source";
export type RemoteRuleType = "remote";

export type RuleMemoryGuard = {
  max_enabled_sets: number;
  max_total_bytes: number;
  max_set_bytes: number;
  warn_total_bytes: number;
  warn_enabled_sets: number;
};

export type RemoteRuleSet = {
  id: string;
  name: string;
  enabled: boolean;
  kind: RemoteRuleKind;
  target: RemoteRuleTarget;
  outbound?: "Direct" | "Proxy" | "Block" | string;
  format?: RemoteRuleFormat;
  type?: RemoteRuleType;
  url: string;
  download_detour?: "Direct" | "Proxy" | "Block" | string;
  update_interval?: string;
  estimated_bytes?: number;
  tags?: string[];
  description?: string;
};

export type AdBlockSettings = {
  enabled: boolean;
  mode: "remote" | "off";
  sets: string[];
};

export type RemoteRuleSettings = {
  enabled: boolean;
  default_format: RemoteRuleFormat;
  download_detour: "Direct" | "Proxy" | "Block" | string;
  update_interval: string;
  memory_guard: RuleMemoryGuard;
  sets: RemoteRuleSet[];
  ad_block: AdBlockSettings;
  warnings?: string[];
};

export type RouteExplain = {
  target: string;
  decision: string;
  pool_id?: string;
  pool?: string;
  reason: string;
  matched?: string;
  matched_rule?: {
    id: string;
    source: RuleSource;
    kind: RuleKind;
    value: string;
    pool_id: string;
  };
  binding?: {
    id?: string;
    pool: string;
    generation?: number;
    smart_node?: string;
    executor?: string;
    core?: CoreID;
    worker?: string;
    target?: string;
  };
  dns_path?: string[];
  chain?: string[];
};

// ===== Connections =====

export type ConnectionItem = {
  id: string;
  metadata: {
    host?: string;
    dnsHosts?: string[];
    network?: string;
    type?: string;
    sourceIP?: string;
    destinationIP?: string;
    sourcePort?: string | number;
    destinationPort?: string | number;
    [key: string]: unknown;
  };
  upload: number;
  download: number;
  chains: string[];
  start?: string;
  pool: string;
  generation: number;
  stale_generation: boolean;
  smart_node: string;
  worker: string;
  core: CoreID | "";
};

export type ConnectionsData = {
  connections: ConnectionItem[];
  download_total: number;
  upload_total: number;
  memory: number;
};

export type ConnectionCloseRequest =
  | { scope: "single"; id: string; confirm?: boolean }
  | { scope: "binding"; pool: string; confirm: true }
  | { scope: "all"; confirm: true };

export type ConnectionCloseResult = {
  scope: ConnectionCloseRequest["scope"];
  closed: number;
  closed_exact: boolean;
  pool?: string;
  id?: string;
};

// ===== System =====

export type ServiceCommandResult = {
  ok: boolean;
  argv?: string[];
  output?: string;
  error?: string;
};

export type SystemMemory = {
  total_bytes: number;
  free_bytes: number;
  available_bytes: number;
  used_bytes: number;
};

export type SystemDisk = {
  path: string;
  total_bytes: number;
  free_bytes: number;
  used_bytes: number;
  used_percent: number;
};

export type SystemService = {
  name: string;
  status: string;
  result?: ServiceCommandResult;
};

export type SystemProcess = {
  name: string;
  running: boolean;
  pids: string[];
  result?: ServiceCommandResult;
};

export type SystemStatus = {
  runtime: {
    phase: string;
    listen_addr: string;
    config_epoch: number;
    rescue_active: boolean;
  };
  memory?: SystemMemory;
  disks: SystemDisk[];
  services: SystemService[];
  processes: SystemProcess[];
  warnings: string[];
  checked_at: string;
};

export type SystemLogs = {
  source: string;
  limit: number;
  lines: string[];
  result: string;
  time: string;
};

export type SystemServiceName = string;
export type SystemServiceAction = "restart" | "reload";

// ===== Wi-Fi =====

export type WifiIface = {
  section: string;
  ifname?: string;
  ssid?: string;
  mode?: string;
  encryption?: string;
  disabled: boolean;
  network?: string;
  band?: string;
  channel?: string;
  htmode?: string;
};

export type WifiRadio = {
  id: string;
  up: boolean;
  pending: boolean;
  disabled: boolean;
  band?: string;
  channel?: string;
  htmode?: string;
  country?: string;
  txpower?: number;
  interfaces: WifiIface[];
};

export type WifiStatus = {
  radios: WifiRadio[];
  interfaces: WifiIface[];
};

export type WifiNetwork = {
  bssid: string;
  ssid: string;
  channel: number;
  signal_dbm: number;
  quality: string;
  encryption: string;
};

export type WifiScanResult = {
  interface: string;
  networks: WifiNetwork[];
};

export type WifiClient = {
  mac: string;
  interface?: string;
  ssid?: string;
  signal_dbm: number;
  hostname?: string;
  ip?: string;
  rx_rate?: number;
  tx_rate?: number;
  rx_bytes?: number;
  tx_bytes?: number;
  authorized?: boolean;
};

export type WifiClientsResult = {
  interface: string;
  interfaces?: string[];
  clients: WifiClient[];
};

export type WifiConfigNetwork = {
  section: string;
  device?: string;
  ssid?: string;
  mode?: string;
  encryption?: string;
  key_configured: boolean;
  disabled: boolean;
  network?: string;
  band?: string;
  channel?: string;
  htmode?: string;
  country?: string;
};

export type WifiConfigDevice = {
  section: string;
  band?: string;
  channel?: string;
  htmode?: string;
  country?: string;
  txpower?: string;
  disabled: boolean;
};

export type WifiConfig = {
  networks: WifiConfigNetwork[];
  devices: WifiConfigDevice[];
};

export type WifiConfigUpdate = {
  section: string;
  ssid?: string;
  encryption?: string;
  key?: string;
  disabled?: boolean;
};

export type WifiConfigUpdateResult = {
  section: string;
  status: string;
  changed: string[];
};

// ===== Snapshots =====

export type Snapshot = {
  id: string;
  kind: string;
  description: string;
  topology_hash: string;
  saved_at: string;
  last_good_captured: boolean;
  provider_count: number;
  business_pool_count: number;
  last_good_count: number;
};

// ===== Backups =====

export type Backup = {
  id: string;
  path: string;
  size: number;
  sha256: string;
  metadata?: Record<string, unknown>;
  created_at: string;
};

// ===== Scheduler =====

export type SchedulerTaskType = string;

export type SchedulerTask = {
  id: string;
  name: string;
  type: SchedulerTaskType | string;
  enabled: boolean;
  schedule: string;
  spec?: Record<string, unknown>;
  running: boolean;
  last_run?: string;
  last_status?: string;
  last_output?: string;
  next_run?: string;
};

export type SchedulerRunResult = {
  result: {
    task: string;
    started_at: string;
    ended_at: string;
  } & Record<string, unknown>;
  task: SchedulerTask;
};
