// 全部 React Query 集成
// Phase 1 配置 dirty / dry-run / apply 工作流：
//   1. 改 desired（gateway mode / DNS / 业务池 / 路由等） → 配置变 dirty
//   2. POST /api/config/dryrun（带 op_id） → 拿到候选与计划
//   3. 用户确认 → POST /api/config/apply（带 dryrun_id） → 落地
//   4. 也可 POST /api/config/discard 放弃
// Phase 2/3 加上 Providers / Nodes / Pools / Proxy Groups / Failover / DNS / Rules / Connections

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { normalizeSmartNode, normalizeSmartNodes } from "./nodeResponse";
import { normalizeConnections } from "./connectionResponse";
import { api, makeOpId } from "./api";
import type {
  ConfigState,
  Overview,
  GatewayMode,
  DryRunResult,
  ApplyResult,
  RuntimeStatus,
  Provider,
  ProviderChange,
  ProviderRefreshLog,
  ProviderRefreshResult,
  SmartNode,
  NodeHealth,
  CoreID,
  CoreInfo,
  CoreChoiceResult,
  BusinessPool,
  BusinessPoolChange,
  ProxyGroup,
  CutoverResult,
  CutoverSource,
  FailoverEvent,
  FailoverRunResult,
  FailoverState,
  DnsSettings,
  RouteRule,
  RulesData,
  RemoteRuleSettings,
  RouteExplain,
  ConnectionsData,
  ConnectionCloseRequest,
  ConnectionCloseResult,
  SystemStatus,
  SystemLogs,
  SystemServiceName,
  SystemServiceAction,
  WifiStatus,
  WifiScanResult,
  WifiClientsResult,
  WifiConfig,
  WifiConfigUpdate,
  WifiConfigUpdateResult,
  Snapshot,
  Backup,
  SchedulerTask,
  SchedulerRunResult,
  WorkerStatus,
  WorkerEvent,
} from "./types";

export const keys = {
  overview: ["overview"] as const,
  configState: ["config", "state"] as const,
  runtimeStatus: ["runtime", "status"] as const,
  gatewayMode: ["gateway", "mode"] as const,
  providers: ["providers"] as const,
  providerLogs: ["providers", "logs"] as const,
  nodes: ["nodes"] as const,
  cores: ["cores"] as const,
  workers: ["workers"] as const,
  workerEvents: (id: string) => ["workers", id, "events"] as const,
  smartNode: (id: string) => ["nodes", id] as const,
  pools: ["business-pools"] as const,
  proxyGroups: ["proxy-groups"] as const,
  failoverEvents: ["proxy-groups", "failover", "events"] as const,
  failoverState: ["proxy-groups", "failover", "state"] as const,
  dns: ["dns"] as const,
  rules: ["rules"] as const,
  ruleConflicts: ["rules", "conflicts"] as const,
  ruleSets: ["rule-sets"] as const,
  connections: ["connections"] as const,
  systemStatus: ["system", "status"] as const,
  systemLogs: ["system", "logs"] as const,
  wifiStatus: ["wifi", "status"] as const,
  wifiClients: ["wifi", "clients"] as const,
  wifiConfig: ["wifi", "config"] as const,
  snapshots: ["snapshots"] as const,
  scheduler: ["scheduler"] as const,
};

// ===== Phase 1 =====

export function useOverview() {
  return useQuery({
    queryKey: keys.overview,
    queryFn: () => api.get<Overview>("/overview"),
    staleTime: 5_000,
    refetchInterval: 15_000,
  });
}

export function useConfigState() {
  return useQuery({
    queryKey: keys.configState,
    queryFn: () => api.get<ConfigState>("/config/state"),
    staleTime: 5_000,
  });
}

export function useRuntimeStatus() {
  return useQuery({
    queryKey: keys.runtimeStatus,
    queryFn: () => api.get<RuntimeStatus>("/runtime/status"),
    staleTime: 5_000,
    refetchInterval: 10_000,
  });
}

export function useGatewayMode() {
  return useQuery({
    queryKey: keys.gatewayMode,
    queryFn: () => api.get<{ mode: GatewayMode }>("/gateway/mode"),
    staleTime: 5_000,
  });
}

export function useSetGatewayMode() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (mode: GatewayMode) =>
      api.post<ConfigState>("/gateway/mode", { mode }),
    onSuccess: (state) => {
      qc.setQueryData(keys.configState, state);
      qc.setQueryData(keys.gatewayMode, { mode: state.gateway_mode });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useDryRun() {
  return useMutation({
    mutationFn: (opts?: { opId?: string }) =>
      api.post<DryRunResult>("/config/dryrun", { op_id: opts?.opId ?? makeOpId("config") }),
  });
}

export function useApply() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { dryrunId: string; description?: string }) =>
      api.post<ApplyResult>("/config/apply", {
        dryrun_id: args.dryrunId,
        description: args.description,
      }),
    onSuccess: () => {
      // dirty 已应用，相关都重新拉
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
      qc.invalidateQueries({ queryKey: keys.runtimeStatus });
      qc.invalidateQueries({ queryKey: keys.gatewayMode });
      qc.invalidateQueries({ queryKey: keys.dns });
      qc.invalidateQueries({ queryKey: keys.rules });
      qc.invalidateQueries({ queryKey: keys.ruleSets });
      qc.invalidateQueries({ queryKey: keys.pools });
      qc.invalidateQueries({ queryKey: keys.proxyGroups });
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.workers });
    },
  });
}

export function useDiscard() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<unknown>("/config/discard", {}),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

// ===== Providers =====

export function useProviders() {
  return useQuery({
    queryKey: keys.providers,
    queryFn: () => api.get<{ providers: Provider[] }>("/providers"),
    staleTime: 5_000,
  });
}

export function useProvidersDryrun() {
  return useMutation({
    mutationFn: (args: { changes: ProviderChange[]; opId?: string }) =>
      api.post<DryRunResult>("/providers/dryrun", {
        op_id: args.opId ?? makeOpId("provider"),
        changes: args.changes,
      }),
  });
}

export function useProvidersApply() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { changes: ProviderChange[]; opId?: string }) =>
      api.post<ApplyResult>("/providers/apply", {
        op_id: args.opId ?? makeOpId("provider"),
        changes: args.changes,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.providers });
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useProviderRefreshAll() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<ProviderRefreshResult>("/providers/refresh-all", {}),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.providers });
      qc.invalidateQueries({ queryKey: keys.providerLogs });
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useProviderRefresh() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (providerId: string) =>
      api.post<ProviderRefreshResult>(`/providers/${encodeURIComponent(providerId)}/refresh`, {}),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.providers });
      qc.invalidateQueries({ queryKey: keys.providerLogs });
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useProviderLogs() {
  return useQuery({
    queryKey: keys.providerLogs,
    queryFn: () => api.get<{ logs: ProviderRefreshLog[] }>("/providers/logs"),
    staleTime: 5_000,
    refetchInterval: 20_000,
  });
}

// ===== Nodes =====

export function useSmartNodes() {
  return useQuery({
    queryKey: keys.nodes,
    queryFn: () => api.get<{ nodes: SmartNode[] }>("/nodes"),
    select: normalizeSmartNodes,
    staleTime: 5_000,
  });
}

export const useNodes = useSmartNodes;

export function useCores() {
  return useQuery({
    queryKey: keys.cores,
    queryFn: () => api.get<CoreInfo[]>("/cores"),
    staleTime: 60_000,
  });
}

// ===== Workers =====

export function useWorkers() {
  return useQuery({
    queryKey: keys.workers,
    queryFn: () => api.get<{ workers: WorkerStatus[] }>("/workers"),
    staleTime: 5_000,
    refetchInterval: 10_000,
  });
}

export function useWorkerEvents(workerId: string | null) {
  return useQuery({
    queryKey: keys.workerEvents(workerId || "none"),
    queryFn: () =>
      api.get<{ worker_id: string; events: WorkerEvent[] }>(
        `/workers/${encodeURIComponent(workerId || "")}/events`,
      ),
    enabled: Boolean(workerId),
    staleTime: 2_000,
    refetchInterval: workerId ? 5_000 : false,
  });
}

export function useSmartNode(nodeId: string) {
  return useQuery({
    queryKey: keys.smartNode(nodeId),
    queryFn: () => api.get<SmartNode>(`/nodes/${encodeURIComponent(nodeId)}`),
    select: normalizeSmartNode,
    enabled: nodeId.length > 0,
    staleTime: 5_000,
  });
}

export function useSetCoreChoice() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { nodeId: string; core: CoreID; confirmDegraded?: boolean; opId?: string }) =>
      api.put<CoreChoiceResult>(`/nodes/${encodeURIComponent(args.nodeId)}/core-choice`, {
        core: args.core,
        confirm_degraded: args.confirmDegraded ?? false,
        op_id: args.opId ?? makeOpId("node-core"),
      }),
    retry: false,
    onSuccess: (result, variables) => {
      qc.setQueryData<SmartNode>(keys.smartNode(variables.nodeId), (current) => ({
        ...result.smart_node,
        health: result.smart_node.health ?? current?.health,
      }));
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.smartNode(variables.nodeId) });
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useNodeDelay() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { nodeId: string; url?: string; timeoutMs?: number; signal?: AbortSignal }) =>
      api.post<{ node_id: string; health: NodeHealth }>(
        `/nodes/${encodeURIComponent(args.nodeId)}/delay`,
        {
          url: args.url ?? "https://www.gstatic.com/generate_204",
          timeout_ms: args.timeoutMs ?? 5000,
        },
        { signal: args.signal },
      ),
    retry: false,
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.smartNode(variables.nodeId) });
    },
  });
}

export function useAllNodeDelay() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (signal: AbortSignal) => api.post<{ delay_test: { total: number; tested: number; ok: number; failed: number; unknown: number } }>("/nodes/delay-all", {}, { signal }),
    retry: false,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.scheduler });
    },
  });
}

// ===== Business Pools =====

export function useBusinessPools() {
  return useQuery({
    queryKey: keys.pools,
    queryFn: () => api.get<{ business_pools: BusinessPool[] }>("/business-pools"),
    staleTime: 5_000,
  });
}

export function useBusinessPoolsDryrun() {
  return useMutation({
    mutationFn: (args: { changes: BusinessPoolChange[]; opId?: string }) =>
      api.post<DryRunResult>("/business-pools/dryrun", {
        op_id: args.opId ?? makeOpId("pool"),
        changes: args.changes,
      }),
  });
}

export function useBusinessPoolsApply() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { changes: BusinessPoolChange[]; opId?: string }) =>
      api.post<ApplyResult>("/business-pools/apply", {
        op_id: args.opId ?? makeOpId("pool"),
        changes: args.changes,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.pools });
      qc.invalidateQueries({ queryKey: keys.proxyGroups });
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

// ===== Proxy Groups =====

export function useProxyGroups() {
  return useQuery({
    queryKey: keys.proxyGroups,
    queryFn: () => api.get<{ groups: ProxyGroup[] }>("/proxy-groups"),
    staleTime: 5_000,
    refetchInterval: 20_000,
  });
}

export function useCutover() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: {
      poolId: string;
      targetSmartNode: string;
      source: CutoverSource;
      expectedConfigEpoch: number;
      opId?: string;
    }) =>
      api.post<CutoverResult>(`/proxy-groups/${encodeURIComponent(args.poolId)}/select`, {
        target_smart_node: args.targetSmartNode,
        source: args.source,
        expected_config_epoch: args.expectedConfigEpoch,
        op_id: args.opId ?? makeOpId(`cutover-${args.poolId}`),
      }),
    retry: false,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: keys.proxyGroups });
      qc.invalidateQueries({ queryKey: keys.runtimeStatus });
      qc.invalidateQueries({ queryKey: keys.failoverState });
      qc.invalidateQueries({ queryKey: keys.failoverEvents });
    },
  });
}

// ===== Failover =====

export function useFailoverState() {
  return useQuery({
    queryKey: keys.failoverState,
    queryFn: () => api.get<FailoverState>("/proxy-groups/failover/state"),
    staleTime: 5_000,
  });
}

export function useFailoverEvents() {
  return useQuery({
    queryKey: keys.failoverEvents,
    queryFn: () => api.get<{ events: FailoverEvent[] }>("/proxy-groups/failover/events"),
    staleTime: 5_000,
    refetchInterval: 30_000,
  });
}

export function useFailoverRun() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<FailoverRunResult>("/proxy-groups/failover/run", {}),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.failoverState });
      qc.invalidateQueries({ queryKey: keys.failoverEvents });
      qc.invalidateQueries({ queryKey: keys.proxyGroups });
    },
  });
}

export function useFailoverEnable() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (enabled: boolean) =>
      api.post<FailoverState>("/proxy-groups/failover/enable", { enabled }),
    onSuccess: (state) => {
      qc.setQueryData(keys.failoverState, state);
    },
  });
}

export function useFailoverLock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { poolId: string; reason?: string }) =>
      api.post<FailoverState>("/proxy-groups/failover/lock", {
        pool_id: args.poolId,
        reason: args.reason ?? "",
      }),
    onSuccess: (state) => qc.setQueryData(keys.failoverState, state),
  });
}

export function useFailoverUnlock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (poolId: string) =>
      api.post<FailoverState>("/proxy-groups/failover/unlock", { pool_id: poolId }),
    onSuccess: (state) => qc.setQueryData(keys.failoverState, state),
  });
}

export function useFailoverRevert() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (poolId: string) =>
      api.post<unknown>("/proxy-groups/failover/revert", { pool_id: poolId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.failoverState });
      qc.invalidateQueries({ queryKey: keys.failoverEvents });
      qc.invalidateQueries({ queryKey: keys.proxyGroups });
    },
  });
}

// ===== DNS =====

export function useDns() {
  return useQuery({
    queryKey: keys.dns,
    queryFn: () => api.get<DnsSettings>("/dns"),
    staleTime: 5_000,
  });
}

export function useDnsUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (settings: DnsSettings) => api.put<DnsSettings>("/dns", settings),
    onSuccess: (data) => {
      qc.setQueryData(keys.dns, data);
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

// ===== Routes / Rules =====

export function useRules() {
  return useQuery({
    queryKey: keys.rules,
    queryFn: () => api.get<RulesData>("/rules"),
    staleTime: 5_000,
  });
}

export function useRulesUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (rules: RouteRule[]) => api.put<RulesData>("/rules", { rules }),
    onSuccess: (data) => {
      qc.setQueryData(keys.rules, data);
      qc.setQueryData(keys.ruleConflicts, data.conflicts);
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useRuleConflicts() {
  return useQuery({
    queryKey: keys.ruleConflicts,
    queryFn: () => api.get<RulesData["conflicts"]>("/rules/conflicts"),
    staleTime: 5_000,
  });
}

export function useResolveRuleConflict() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { ruleKey: string; keepPool: string; opId?: string }) =>
      api.post<RulesData>("/rules/resolve-conflict", {
        rule_key: args.ruleKey,
        keep_pool: args.keepPool,
        op_id: args.opId ?? makeOpId("rule-resolve"),
      }),
    onSuccess: (data) => {
      qc.setQueryData(keys.rules, data);
      qc.setQueryData(keys.ruleConflicts, data.conflicts);
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useRuleSets() {
  return useQuery({
    queryKey: keys.ruleSets,
    queryFn: () => api.get<RemoteRuleSettings>("/rule-sets"),
    staleTime: 5_000,
  });
}

export function useRuleSetsUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (settings: RemoteRuleSettings) =>
      api.put<RemoteRuleSettings>("/rule-sets", settings),
    onSuccess: (data) => {
      qc.setQueryData(keys.ruleSets, data);
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
    },
  });
}

export function useExplainRoute() {
  return useMutation({
    mutationFn: (target: string) =>
      api.post<RouteExplain>("/routes/explain", { target }),
  });
}

// ===== Connections =====

export function useConnections(intervalMs: number = 5_000) {
  return useQuery({
    queryKey: keys.connections,
    queryFn: () => api.get<ConnectionsData>("/connections"),
    select: normalizeConnections,
    staleTime: 1_000,
    refetchInterval: intervalMs,
  });
}

export function useCloseConnections() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (request: ConnectionCloseRequest) =>
      api.post<ConnectionCloseResult>("/connections/close", request),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.connections });
    },
  });
}

// ===== System =====

export function useSystemStatus() {
  return useQuery({
    queryKey: keys.systemStatus,
    queryFn: () => api.get<SystemStatus>("/system/status"),
    staleTime: 5_000,
    refetchInterval: 15_000,
  });
}

export function useSystemLogs(source: string, limit = 100) {
  return useQuery({
    queryKey: [...keys.systemLogs, source, limit],
    queryFn: () => api.get<SystemLogs>(`/system/logs?source=${source}&limit=${limit}`),
    staleTime: 5_000,
  });
}

export function useSystemServiceCommand() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { service: SystemServiceName; action: SystemServiceAction }) =>
      api.post<unknown>(`/system/service/${args.service}/${args.action}`, { confirm: true }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.systemStatus });
    },
  });
}

export function useSystemDnsFlush() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<unknown>("/system/dns/flush", { confirm: true }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.systemStatus });
    },
  });
}

export function useSystemReboot() {
  return useMutation({
    mutationFn: () => api.post<unknown>("/system/reboot", { confirm: true }),
  });
}

// ===== Wi-Fi =====

export function useWifiStatus() {
  return useQuery({
    queryKey: keys.wifiStatus,
    queryFn: () => api.get<WifiStatus>("/wifi/status"),
    staleTime: 10_000,
    refetchInterval: 30_000,
  });
}

export function useWifiScan() {
  return useMutation({
    mutationFn: (iface?: string) =>
      api.post<WifiScanResult>("/wifi/scan", {
        interface: iface && iface.trim() ? iface.trim() : "auto",
      }),
  });
}

export function useWifiClients(iface = "all") {
  const q = iface && iface.trim() ? iface.trim() : "all";
  return useQuery({
    queryKey: [...keys.wifiClients, q],
    queryFn: () => api.get<WifiClientsResult>(`/wifi/clients?interface=${encodeURIComponent(q)}`),
    staleTime: 10_000,
    refetchInterval: 30_000,
  });
}

export function useWifiConfig() {
  return useQuery({
    queryKey: keys.wifiConfig,
    queryFn: () => api.get<WifiConfig>("/wifi/config"),
    staleTime: 10_000,
  });
}

export function useWifiConfigUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (update: WifiConfigUpdate & { confirm: true }) =>
      api.put<WifiConfigUpdateResult>("/wifi/config", update),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.wifiConfig });
      qc.invalidateQueries({ queryKey: keys.wifiStatus });
      qc.invalidateQueries({ queryKey: keys.wifiClients });
    },
  });
}

// ===== Snapshots =====

export function useSnapshots() {
  return useQuery({
    queryKey: keys.snapshots,
    queryFn: () => api.get<{ snapshots: Snapshot[] }>("/snapshots"),
    staleTime: 5_000,
  });
}

export function useSnapshotCreate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { id?: string; kind?: string; description?: string }) =>
      api.post<Snapshot>("/snapshots", {
        id: args.id ?? `snap-${Date.now()}`,
        kind: args.kind ?? "manual",
        description: args.description ?? "",
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.snapshots });
    },
  });
}

export function useSnapshotRollbackDryrun() {
  return useMutation({
    mutationFn: (args: { dryrunId: string; snapshotId: string }) =>
      api.post<DryRunResult>("/snapshots/rollback/dryrun", {
        dryrun_id: args.dryrunId,
        snapshot_id: args.snapshotId,
      }),
  });
}

export function useSnapshotRollbackApply() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { dryrunId: string; snapshotId: string }) =>
      api.post<ApplyResult>("/snapshots/rollback/apply", {
        dryrun_id: args.dryrunId,
        snapshot_id: args.snapshotId,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.snapshots });
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
      qc.invalidateQueries({ queryKey: keys.providers });
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.pools });
    },
  });
}

// ===== Backups =====

export function useBackupCreate() {
  return useMutation({
    mutationFn: (args?: { opId?: string; kind?: string }) =>
      api.post<Backup>("/backups/create", {
        op_id: args?.opId ?? makeOpId("backup"),
        kind: args?.kind ?? "manual",
      }),
  });
}

export function useBackupRestore() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { path: string; opId?: string }) =>
      api.post<unknown>("/backups/restore", {
        op_id: args.opId ?? makeOpId("restore"),
        path: args.path,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.configState });
      qc.invalidateQueries({ queryKey: keys.overview });
      qc.invalidateQueries({ queryKey: keys.providers });
      qc.invalidateQueries({ queryKey: keys.nodes });
      qc.invalidateQueries({ queryKey: keys.pools });
    },
  });
}

// ===== Scheduler =====

export function useScheduler() {
  return useQuery({
    queryKey: keys.scheduler,
    queryFn: () => api.get<{ tasks: SchedulerTask[] }>("/scheduler"),
    staleTime: 5_000,
    refetchInterval: 15_000,
  });
}

export function useSchedulerTaskCreate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (task: Omit<SchedulerTask, "running" | "last_run" | "last_status" | "last_output" | "next_run">) =>
      api.post<SchedulerTask>("/scheduler", task),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.scheduler });
    },
  });
}

export function useSchedulerTaskUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { id: string; task: Partial<SchedulerTask> }) =>
      api.put<SchedulerTask>(`/scheduler/${encodeURIComponent(args.id)}`, args.task),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.scheduler });
    },
  });
}

export function useSchedulerTaskDelete() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.del<unknown>(`/scheduler/${encodeURIComponent(id)}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.scheduler });
    },
  });
}

export function useSchedulerTaskRun() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { id: string; confirm?: boolean }) =>
      api.post<SchedulerRunResult>(
        `/scheduler/${encodeURIComponent(args.id)}/run`,
        args.confirm ? { confirm: true } : {}
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: keys.scheduler });
    },
  });
}

// ===== Password =====

export function usePasswordChange() {
  return useMutation({
    mutationFn: (args: { oldPassword: string; newPassword: string }) =>
      api.put<unknown>("/auth/password", {
        old_password: args.oldPassword,
        new_password: args.newPassword,
      }),
  });
}
