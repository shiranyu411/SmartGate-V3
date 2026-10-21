import { Fragment, useEffect, useMemo, useRef, useState, type KeyboardEvent, type MouseEvent, type ReactNode } from "react";
import {
  useCores,
  useNodeDelay,
  useAllNodeDelay,
  useProviders,
  useSetCoreChoice,
  useSmartNode,
  useSmartNodes,
} from "../lib/queries";
import { capabilityStatusLabel } from "../lib/statusText";
import { Badge, Dot } from "../ui/Badge";
import { Card, Eyebrow, SectionHeader } from "../ui/Card";
import { Segmented } from "../ui/Segmented";
import { Input, Select } from "../ui/Field";
import { Button } from "../ui/Button";
import { Modal } from "../ui/Modal";
import { ErrorText } from "../ui/ErrorText";
import { coreLabel, coreLabelMap, type CoreLabelMap } from "../lib/coreLabels";
import type {
  CapabilityStatus,
  CoreCapability,
  CoreID,
  SmartNode,
} from "../lib/types";
import { coreIDsForNode, filterSmartNodes, groupSmartNodesByProvider, requiresDegradedConfirmation, selectableCores, type SmartNodeFilters } from "./nodeState";
import { serif, sans, eyebrowStyle } from "../ui/tokens";

const noSmartNodes: SmartNode[] = [];
const emptyFilters: SmartNodeFilters = {
  keyword: "",
  provider: "",
  protocol: "",
  core: "",
  capability: "",
  health: "",
};

export function NodesPage() {
  const nodesQuery = useSmartNodes();
  const providersQuery = useProviders();
  const coresQuery = useCores();
  const delay = useNodeDelay();
  const delayAll = useAllNodeDelay();
  const [filters, setFilters] = useState<SmartNodeFilters>(emptyFilters);
  const [selectedNodeID, setSelectedNodeID] = useState<string | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const [pendingNodeIDs, setPendingNodeIDs] = useState<Set<string>>(new Set());
  const [batch, setBatch] = useState<{ total: number; done: number; failed: number; unknown: number; running: boolean } | null>(null);
  const batchController = useRef<AbortController | null>(null);

  useEffect(() => () => batchController.current?.abort(), []);

  const nodes = nodesQuery.data?.nodes ?? noSmartNodes;
  const registeredCoreIDs = useMemo(() => coresQuery.data?.map((core) => core.id) ?? [], [coresQuery.data]);
  const coreLabels = useMemo(() => coreLabelMap(coresQuery.data), [coresQuery.data]);
  const coreIDs = useMemo(() => mergeCoreIDs(registeredCoreIDs, nodes), [registeredCoreIDs, nodes]);
  const providerNames = useMemo(
    () => new Map((providersQuery.data?.providers ?? []).map((provider) => [provider.id, provider.name])),
    [providersQuery.data?.providers],
  );
  const providerIDs = useMemo(
    () => [...new Set([
      ...(providersQuery.data?.providers ?? []).filter((provider) => provider.enabled).map((provider) => provider.id),
      ...nodes.flatMap((node) => node.aliases.map((alias) => alias.provider_id)),
    ])],
    [nodes, providersQuery.data?.providers],
  );
  const protocols = useMemo(
    () => [...new Set(nodes.map((node) => node.identity.protocol).filter(Boolean))].sort(),
    [nodes],
  );
  const filtered = useMemo(() => filterSmartNodes(nodes, filters), [nodes, filters]);
  const stats = useMemo(() => ({
    total: nodes.length,
    assigned: nodes.filter((node) => node.core_choice !== null).length,
    healthy: nodes.filter((node) => node.health?.status === "ok").length,
    missing: nodes.filter((node) => node.source_missing).length,
  }), [nodes]);

  function updateFilter<K extends keyof SmartNodeFilters>(key: K, value: SmartNodeFilters[K]) {
    setFilters((current) => ({ ...current, [key]: value }));
  }

  async function measureNode(nodeID: string, signal?: AbortSignal): Promise<boolean | null> {
    setPendingNodeIDs((current) => new Set(current).add(nodeID));
    try {
      const result = await delay.mutateAsync({ nodeId: nodeID, signal });
      return result.health.status === "ok";
    } catch (error) {
      if (signal?.aborted) return null;
      setActionError((current: unknown) => current ?? error);
      return false;
    } finally {
      setPendingNodeIDs((current) => {
        const next = new Set(current);
        next.delete(nodeID);
        return next;
      });
    }
  }

  async function testDelay(nodeID: string) {
    if (batchController.current) return;
    setActionError(null);
    await measureNode(nodeID);
  }

  async function testAllDelay() {
    if (batchController.current || pendingNodeIDs.size || !nodes.length) return;
    const controller = new AbortController();
    batchController.current = controller;
    const total = nodes.filter((node) => !node.source_missing).length;
    setActionError(null);
    setBatch({ total, done: 0, failed: 0, unknown: 0, running: true });
    try {
      const result = await delayAll.mutateAsync(controller.signal);
      setBatch({ total: result.delay_test.total, done: result.delay_test.total, failed: result.delay_test.failed, unknown: result.delay_test.unknown, running: false });
    } catch (error) {
      if (!controller.signal.aborted) setActionError(error);
    } finally {
      batchController.current = null;
      setBatch((current) => current && ({ ...current, running: false }));
    }
  }

  return (
    <div className="space-y-9 max-w-[1380px]">
      <section className="space-y-3">
        <Eyebrow>节点 · SmartNodes</Eyebrow>
        <h1
          className="text-[28px] md:text-[36px] leading-[1.15] font-medium"
          style={{ ...serif, color: "var(--fg-strong)", letterSpacing: 0 }}
        >
          SmartNode 目录
        </h1>
        <div className="flex flex-wrap items-center gap-3 pt-2">
          <Badge tone="info">总计 · {stats.total}</Badge>
          <Badge tone={stats.assigned === stats.total ? "ok" : "warn"}>已选核心 · {stats.assigned}</Badge>
          <Badge tone="ok">健康 · {stats.healthy}</Badge>
          {stats.missing > 0 ? <Badge tone="warn">来源缺失 · {stats.missing}</Badge> : null}
        </div>
      </section>

      <section className="space-y-4">
        <SectionHeader
          eyebrow="目录"
          title="节点列表"
          right={
            <div className="flex flex-wrap items-center justify-end gap-3">
              <span className="text-xs tabular-nums" style={{ color: "var(--muted)", ...sans }}>{filtered.length} / {nodes.length}</span>
              {batch?.running ? (
                <Button size="sm" variant="ghost" onClick={() => batchController.current?.abort()}>停止测速</Button>
              ) : (
                <Button size="sm" onClick={testAllDelay} disabled={!nodes.length || pendingNodeIDs.size > 0}>全部测速</Button>
              )}
            </div>
          }
        />

        {batch ? (
          <p className="text-xs tabular-nums" role="status" style={{ ...sans, color: "var(--muted)" }}>
            {batch.running ? "测速中" : batch.done === batch.total ? "测速完成" : "测速已停止"}
            {" · "}{batch.done} / {batch.total}
            {batch.failed > 0 ? ` · 失败 ${batch.failed}` : ""}
            {batch.unknown > 0 ? ` · 未完成 ${batch.unknown}` : ""}
          </p>
        ) : null}

        <div className="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-[minmax(220px,1.4fr)_repeat(5,minmax(130px,0.7fr))_auto] gap-3 items-end">
          <CompactFilter label="搜索">
            <Input
              value={filters.keyword}
              onChange={(event) => updateFilter("keyword", event.target.value)}
              placeholder="名称 / Server / Alias"
            />
          </CompactFilter>
          <CompactFilter label="Provider">
            <Select value={filters.provider} onChange={(event) => updateFilter("provider", event.target.value)}>
              <option value="">全部来源</option>
              {providerIDs.map((id) => <option key={id} value={id}>{providerNames.get(id) || id}</option>)}
            </Select>
          </CompactFilter>
          <CompactFilter label="协议">
            <Select value={filters.protocol} onChange={(event) => updateFilter("protocol", event.target.value)}>
              <option value="">全部协议</option>
              {protocols.map((protocol) => <option key={protocol} value={protocol}>{protocol}</option>)}
            </Select>
          </CompactFilter>
          <CompactFilter label="执行核心">
            <Select value={filters.core} onChange={(event) => updateFilter("core", event.target.value)}>
              <option value="">全部核心</option>
              {coreIDs.map((core) => <option key={core} value={core}>{coreLabel(core, coreLabels)}</option>)}
              <option value="unset">未设置</option>
            </Select>
          </CompactFilter>
          <CompactFilter label="Capability">
            <Select value={filters.capability} onChange={(event) => updateFilter("capability", event.target.value)}>
              <option value="">全部能力</option>
              <option value="supported">支持</option>
              <option value="degraded">降级</option>
              <option value="unsupported_by_core">不支持</option>
            </Select>
          </CompactFilter>
          <CompactFilter label="健康状态">
            <Select value={filters.health} onChange={(event) => updateFilter("health", event.target.value)}>
              <option value="">全部状态</option>
              <option value="ok">正常</option>
              <option value="failed">失败</option>
              <option value="untested">未测</option>
            </Select>
          </CompactFilter>
          <Button variant="ghost" size="sm" onClick={() => setFilters(emptyFilters)} disabled={!hasActiveFilters(filters)}>
            清除
          </Button>
        </div>

        {actionError ? <Notice tone="err"><ErrorText error={actionError} fallback="节点测速失败" showDiagnostic /></Notice> : null}

        <Card>
          {nodesQuery.isLoading ? (
            <EmptyState>正在拉取 SmartNode…</EmptyState>
          ) : nodesQuery.error ? (
            <EmptyState tone="err"><ErrorText error={nodesQuery.error} fallback="节点列表加载失败" showDiagnostic /></EmptyState>
          ) : filtered.length === 0 ? (
            <EmptyState>未匹配到节点。</EmptyState>
          ) : (
            <NodeTable
              nodes={filtered}
              providerNames={providerNames}
              providerIDs={providerIDs}
              coreLabels={coreLabels}
              pendingNodeIDs={pendingNodeIDs}
              delayDisabled={batch?.running ?? false}
              onOpen={setSelectedNodeID}
              onDelay={testDelay}
            />
          )}
        </Card>
      </section>

      {selectedNodeID ? (
        <NodeDetailModal
          nodeID={selectedNodeID}
          providerNames={providerNames}
          registeredCoreIDs={registeredCoreIDs}
          coreLabels={coreLabels}
          onClose={() => setSelectedNodeID(null)}
        />
      ) : null}
    </div>
  );
}

function NodeTable({
  nodes,
  providerNames,
  providerIDs,
  coreLabels,
  pendingNodeIDs,
  delayDisabled,
  onOpen,
  onDelay,
}: {
  nodes: SmartNode[];
  providerNames: Map<string, string>;
  providerIDs: string[];
  coreLabels: CoreLabelMap;
  pendingNodeIDs: Set<string>;
  delayDisabled: boolean;
  onOpen: (id: string) => void;
  onDelay: (id: string) => void;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[1040px] text-sm" style={sans}>
        <thead>
          <tr style={{ borderBottom: "1px solid var(--border)" }}>
            <Th>健康</Th>
            <Th>SmartNode</Th>
            <Th>传输 / 安全</Th>
            <Th>来源</Th>
            <Th>执行核心</Th>
            <Th>Pool</Th>
            <Th align="right">操作</Th>
          </tr>
        </thead>
        <tbody>
          {groupSmartNodesByProvider(nodes, providerIDs).map((group) => (
            <Fragment key={group.providerID}>
              <tr style={{ background: "var(--surface-2)", borderBottom: "1px solid var(--border)" }}>
                <td colSpan={7} className="px-5 py-2 text-xs font-medium" style={{ color: "var(--fg-strong)" }}>
                  {providerNames.get(group.providerID) || group.providerID || "来源缺失"}
                  <span className="ml-2 tabular-nums" style={{ color: "var(--muted)" }}>{group.nodes.length}</span>
                </td>
              </tr>
              {group.nodes.map((node) => (
                <NodeRow
                  key={node.id}
                  node={node}
                  providerNames={providerNames}
                  coreLabels={coreLabels}
                  pending={pendingNodeIDs.has(node.id)}
                  delayDisabled={delayDisabled}
                  onOpen={() => onOpen(node.id)}
                  onDelay={onDelay}
                />
              ))}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function NodeRow({
  node,
  providerNames,
  coreLabels,
  pending,
  delayDisabled,
  onOpen,
  onDelay,
}: {
  node: SmartNode;
  providerNames: Map<string, string>;
  coreLabels: CoreLabelMap;
  pending: boolean;
  delayDisabled: boolean;
  onOpen: () => void;
  onDelay: (id: string) => void;
}) {
  function openFromKeyboard(event: KeyboardEvent<HTMLTableRowElement>) {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      onOpen();
    }
  }

  function delayFromButton(event: MouseEvent<HTMLButtonElement>) {
    event.stopPropagation();
    onDelay(node.id);
  }

  return (
    <tr
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={openFromKeyboard}
      className="cursor-pointer transition-colors hover:bg-[var(--surface-2)] focus:outline-none focus:bg-[var(--surface-2)]"
      style={{ borderBottom: "1px dotted var(--border)" }}
    >
      <Td><HealthBadge node={node} /></Td>
      <Td>
        <div className="min-w-[210px] leading-tight">
          <div className="flex flex-wrap items-center gap-2">
            <span style={{ ...serif, color: "var(--fg-strong)" }}>{node.display_name || node.id}</span>
            {node.aliases.length > 1 ? <Badge tone="muted">{node.aliases.length} aliases</Badge> : null}
            {node.source_missing ? <Badge tone="warn">来源缺失</Badge> : null}
          </div>
          <p className="mt-1 text-xs" style={{ color: "var(--muted)", fontFamily: "var(--font-mono)" }}>
            {node.identity.server}:{node.identity.port}
          </p>
          <p className="mt-1 text-[11px]" style={{ color: "var(--muted)", fontFamily: "var(--font-mono)" }}>
            {node.id} · r{node.revision}
          </p>
        </div>
      </Td>
      <Td>
        <div className="flex flex-wrap items-center gap-1.5">
          <Badge tone="info">{node.identity.protocol}</Badge>
          {node.identity.transport.type ? <Badge tone="muted">{node.identity.transport.type}</Badge> : null}
          {node.identity.security.type ? <Badge tone="accent">{node.identity.security.type}</Badge> : null}
        </div>
      </Td>
      <Td>
        <div className="max-w-[180px] space-y-1 text-xs" style={{ color: "var(--muted)" }}>
          {node.aliases.slice(0, 2).map((alias) => (
            <p key={`${alias.provider_id}:${alias.raw_tag}`} className="truncate" title={`${providerNames.get(alias.provider_id) || alias.provider_id} · ${alias.raw_tag}`}>
              {providerNames.get(alias.provider_id) || alias.provider_id}
            </p>
          ))}
          {node.aliases.length > 2 ? <p>另 {node.aliases.length - 2} 个来源</p> : null}
        </div>
      </Td>
      <Td><CoreBadge core={node.core_choice} labels={coreLabels} /></Td>
      <Td>
        <span className="tabular-nums" title={node.referenced_by_pools.join(", ")} style={{ color: "var(--fg-strong)" }}>
          {node.referenced_by_pools.length}
        </span>
      </Td>
      <Td align="right">
        <Button
          variant="ghost"
          size="sm"
          loading={pending}
          disabled={delayDisabled}
          onClick={delayFromButton}
          title="测延迟"
        >
          测延迟
        </Button>
      </Td>
    </tr>
  );
}

function NodeDetailModal({
  nodeID,
  providerNames,
  registeredCoreIDs,
  coreLabels,
  onClose,
}: {
  nodeID: string;
  providerNames: Map<string, string>;
  registeredCoreIDs: CoreID[];
  coreLabels: CoreLabelMap;
  onClose: () => void;
}) {
  const query = useSmartNode(nodeID);
  const setCoreChoice = useSetCoreChoice();
  const delay = useNodeDelay();
  const [draftCore, setDraftCore] = useState<CoreID | "">("");
  const [confirmDegraded, setConfirmDegraded] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [result, setResult] = useState<{ affectedPools: string[]; dirty: boolean } | null>(null);

  const node = query.data;
  const detailCoreIDs = useMemo(() => node ? mergeCoreIDsForNode(registeredCoreIDs, node) : registeredCoreIDs, [registeredCoreIDs, node]);
  const selectable = useMemo(() => node ? selectableCores(node) : [], [node]);
  const effectiveDraftCore = draftCore || node?.core_choice || "";
  const requiresConfirm = node ? requiresDegradedConfirmation(node, effectiveDraftCore) : false;
  const coreChanged = Boolean(node && effectiveDraftCore && effectiveDraftCore !== node.core_choice);
  const canSave = Boolean(node && coreChanged && (!requiresConfirm || confirmDegraded));

  async function saveCoreChoice() {
    if (!node || !effectiveDraftCore || !canSave) return;
    setError(null);
    setResult(null);
    try {
      const response = await setCoreChoice.mutateAsync({
        nodeId: node.id,
        core: effectiveDraftCore,
        confirmDegraded: requiresConfirm && confirmDegraded,
      });
      setDraftCore(response.smart_node.core_choice ?? effectiveDraftCore);
      setConfirmDegraded(false);
      setResult({ affectedPools: response.affected_pools ?? [], dirty: response.dirty });
    } catch (requestError) {
      setError(requestError);
    }
  }

  async function testDelay() {
    setError(null);
    try {
      await delay.mutateAsync({ nodeId: nodeID });
    } catch (requestError) {
      setError(requestError);
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      width={920}
      eyebrow="SmartNode 详情"
      title={node?.display_name || "节点详情"}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>关闭</Button>
          <Button onClick={saveCoreChoice} loading={setCoreChoice.isPending} disabled={!canSave}>
            保存核心选择
          </Button>
        </>
      }
    >
      {query.isLoading ? (
        <EmptyState>正在读取节点详情…</EmptyState>
      ) : query.error ? (
        <EmptyState tone="err"><ErrorText error={query.error} fallback="节点详情加载失败" showDiagnostic /></EmptyState>
      ) : node ? (
        <div className="space-y-7">
          {error ? <Notice tone="err"><ErrorText error={error} fallback="节点操作失败" showDiagnostic /></Notice> : null}
          {result ? (
            <Notice tone={result.dirty ? "warn" : "ok"}>
              {result.dirty ? "核心选择已写入 desired，配置待应用。" : "核心选择未改变。"}
              {result.affectedPools.length > 0 ? ` 受影响 Pool：${result.affectedPools.join("、")}。` : " 无 Pool 引用此节点。"}
            </Notice>
          ) : null}

          <section className="space-y-3">
            <DetailHeading title="解析结果" meta={`Revision ${node.revision}`} />
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-x-6 gap-y-4">
              <DetailItem label="协议" value={node.identity.protocol} mono />
              <DetailItem label="Server" value={`${node.identity.server}:${node.identity.port}`} mono />
              <DetailItem label="SecretRef" value={node.identity.secret_ref_masked || "未设置"} mono />
              <DetailItem label="传输" value={node.identity.transport.type || "未设置"} mono />
              <DetailItem label="Path" value={node.identity.transport.path || "未设置"} mono />
              <DetailItem label="Host" value={node.identity.transport.host || "未设置"} mono />
              <DetailItem label="Service Name" value={node.identity.transport.service_name || "未设置"} mono />
              <DetailItem label="安全" value={node.identity.security.type || "未设置"} mono />
              <DetailItem label="SNI" value={node.identity.security.sni || "未设置"} mono />
              <DetailItem label="ALPN" value={node.identity.security.alpn?.join(", ") || "未设置"} mono />
              <DetailItem label="Flow" value={node.identity.security.flow || "未设置"} mono />
              <DetailItem label="Fingerprint" value={node.identity.security.fingerprint || "未设置"} mono />
              <DetailItem label="REALITY Public Key" value={node.identity.security.public_key_masked || "未设置"} mono />
              <DetailItem label="Short ID" value={node.identity.security.short_id || "未设置"} mono />
              <DetailItem label="证书校验" value={node.identity.security.skip_cert_verify ? "跳过" : "校验"} />
              <DetailItem label="证书引用" value={node.identity.security.certificate_masked || "未设置"} mono />
            </div>
          </section>

          <section className="space-y-3 border-t pt-6" style={{ borderColor: "var(--border)" }}>
            <DetailHeading title="Capability" />
            <div className="divide-y" style={{ borderColor: "var(--border)" }}>
              {detailCoreIDs.map((core) => <CapabilityRow key={core} core={core} capability={node.capability[core]} labels={coreLabels} />)}
            </div>
          </section>

          <section className="space-y-4 border-t pt-6" style={{ borderColor: "var(--border)" }}>
            <DetailHeading title="执行核心选择" meta={`引用 ${node.referenced_by_pools.length} 个 Pool`} />
            <Segmented<CoreID | "">
              value={effectiveDraftCore}
              onChange={(core) => {
                setDraftCore(core);
                setConfirmDegraded(false);
                setError("");
                setResult(null);
              }}
              options={detailCoreIDs.map((core) => ({
                key: core,
                label: coreLabel(core, coreLabels),
                disabled: !selectable.includes(core),
                title: capabilityReason(node.capability[core]),
              }))}
            />
            {requiresConfirm ? (
              <label className="flex items-start gap-3 text-sm cursor-pointer" style={{ color: "var(--warning)" }}>
                <input
                  type="checkbox"
                  className="mt-0.5 size-4 accent-[var(--accent)]"
                  checked={confirmDegraded}
                  onChange={(event) => setConfirmDegraded(event.target.checked)}
                />
                <span>确认接受该核心列出的语义损失</span>
              </label>
            ) : null}
            <p className="text-xs" style={{ color: "var(--muted)" }}>
              {node.referenced_by_pools.length > 0
                ? `受影响 Pool：${node.referenced_by_pools.join("、")}`
                : "当前无 Pool 引用"}
            </p>
          </section>

          <section className="space-y-3 border-t pt-6" style={{ borderColor: "var(--border)" }}>
            <DetailHeading title="Alias 与来源历史" />
            {node.aliases.length > 0 ? (
              <div className="overflow-x-auto">
                <table className="w-full min-w-[620px] text-xs" style={sans}>
                  <thead><tr style={{ borderBottom: "1px solid var(--border)" }}><Th>Provider</Th><Th>原始名</Th><Th>首见</Th><Th>最近</Th></tr></thead>
                  <tbody>
                    {node.aliases.map((alias) => (
                      <tr key={`${alias.provider_id}:${alias.raw_tag}`} style={{ borderBottom: "1px dotted var(--border)" }}>
                        <Td>{providerNames.get(alias.provider_id) || alias.provider_id}</Td>
                        <Td><code style={{ fontFamily: "var(--font-mono)" }}>{alias.raw_tag}</code></Td>
                        <Td>{formatTimestamp(alias.first_seen)}</Td>
                        <Td>{formatTimestamp(alias.last_seen)}</Td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : <p className="text-sm" style={{ color: "var(--muted)" }}>来源记录为空。</p>}
          </section>

          <section className="space-y-4 border-t pt-6" style={{ borderColor: "var(--border)" }}>
            <DetailHeading title="健康样本" meta={node.health ? formatTimestamp(node.health.updated_at) : "未测"} />
            {node.health ? (
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-x-6 gap-y-4">
                <DetailItem label="状态" value={node.health.status === "ok" ? "正常" : "失败"} />
                <DetailItem label="延迟" value={node.health.delay_ms === undefined ? "未记录" : `${node.health.delay_ms} ms`} />
                <DetailItem label="Revision" value={String(node.health.context.revision)} mono />
                <DetailItem label="Core" value={node.health.context.core || "未设置"} mono />
                <DetailItem label="Worker" value={node.health.context.worker || "未记录"} mono />
                <DetailItem label="Method" value={node.health.context.method} mono />
                <DetailItem label="测试 URL" value={node.health.test_url || "未记录"} mono />
                <DetailItem label="连续失败" value={String(node.health.consecutive_failures ?? 0)} />
              </div>
            ) : <p className="text-sm" style={{ color: "var(--muted)" }}>暂无健康样本。</p>}
            <div className="flex flex-wrap gap-3">
              <Button variant="ghost" size="sm" loading={delay.isPending} onClick={testDelay}>测延迟</Button>
              <Button variant="ghost" size="sm" disabled title="暂缓：深度测速尚未实现">深度测速</Button>
            </div>
          </section>
        </div>
      ) : null}
    </Modal>
  );
}

function CapabilityRow({ core, capability, labels }: { core: CoreID; capability?: CoreCapability; labels: CoreLabelMap }) {
  const reasons = capability ? capability.reason_texts?.length ? capability.reason_texts : capability.reasons : [];
  return (
    <div className="grid grid-cols-1 sm:grid-cols-[120px_180px_1fr] gap-2 sm:gap-4 py-3 first:pt-0 last:pb-0">
      <code className="text-sm" style={{ fontFamily: "var(--font-mono)", color: "var(--fg-strong)" }}>{coreLabel(core, labels)}</code>
      <div>{capability ? <CapabilityBadge status={capability.status} /> : <Badge tone="muted">未返回</Badge>}</div>
      <div className="space-y-1 text-xs" style={{ color: capability?.status === "supported" ? "var(--muted)" : "var(--fg)" }}>
        {reasons.length > 0 ? reasons.map((reason, index) => (
          <p key={`${reason}:${index}`}>
            {reason}
            {capability?.reason_texts?.[index] && capability.reasons[index] ? (
              <code className="ml-2" style={{ color: "var(--muted)", fontFamily: "var(--font-mono)" }}>{capability.reasons[index]}</code>
            ) : null}
          </p>
        )) : <span>{capability ? "无兼容性损失" : "后端未返回该核心能力"}</span>}
      </div>
    </div>
  );
}

function HealthBadge({ node }: { node: SmartNode }) {
  if (!node.health) return <Badge tone="muted"><Dot tone="muted" />未测</Badge>;
  if (node.health.status === "ok") {
    return <Badge tone="ok"><Dot tone="ok" />正常{node.health.delay_ms === undefined ? "" : ` · ${node.health.delay_ms} ms`}</Badge>;
  }
  return <span title={node.health.error}><Badge tone="err"><Dot tone="err" />失败</Badge></span>;
}

function CoreBadge({ core, labels }: { core: CoreID | null; labels: CoreLabelMap }) {
  if (!core) return <Badge tone="warn">未设置</Badge>;
  return <Badge tone="info">{coreLabel(core, labels)}</Badge>;
}

function CapabilityBadge({ status }: { status: CapabilityStatus }) {
  const tone = status === "supported" ? "ok" : status === "degraded" ? "warn" : "err";
  return <span title={status}><Badge tone={tone}>{capabilityStatusLabel(status)}</Badge></span>;
}

function CompactFilter({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block min-w-0">
      <span className="mb-1.5 block text-[11px]" style={eyebrowStyle}>{label}</span>
      {children}
    </label>
  );
}

function DetailHeading({ title, meta }: { title: string; meta?: string }) {
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-2">
      <h3 className="text-base font-medium" style={{ ...serif, color: "var(--fg-strong)" }}>{title}</h3>
      {meta ? <span className="text-xs" style={{ color: "var(--muted)", ...sans }}>{meta}</span> : null}
    </div>
  );
}

function DetailItem({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="min-w-0">
      <p className="text-[11px]" style={eyebrowStyle}>{label}</p>
      <p className="mt-1 break-words text-sm" style={{ color: "var(--fg-strong)", fontFamily: mono ? "var(--font-mono)" : undefined }}>{value}</p>
    </div>
  );
}

function Notice({ tone, children }: { tone: "ok" | "warn" | "err"; children: ReactNode }) {
  const color = tone === "ok" ? "var(--success)" : tone === "warn" ? "var(--warning)" : "var(--danger)";
  return (
    <div
      className="px-4 py-3 text-sm"
      style={{ color, border: `1px solid color-mix(in srgb, ${color} 35%, transparent)`, background: `color-mix(in srgb, ${color} 9%, transparent)`, borderRadius: "var(--radius-sm)", ...sans }}
    >
      {children}
    </div>
  );
}

function EmptyState({ children, tone = "muted" }: { children: ReactNode; tone?: "muted" | "err" }) {
  return (
    <p className="px-7 py-10 text-sm" style={{ ...sans, color: tone === "err" ? "var(--danger)" : "var(--muted)" }}>
      {children}
    </p>
  );
}

function Th({ children, align }: { children: ReactNode; align?: "right" }) {
  return <th className="px-5 py-3 text-[11px] font-medium" style={{ ...eyebrowStyle, textAlign: align || "left" }}>{children}</th>;
}

function Td({ children, align }: { children: ReactNode; align?: "right" }) {
  return <td className="px-5 py-3 align-top" style={{ textAlign: align || "left" }}>{children}</td>;
}

function capabilityReason(capability?: CoreCapability): string | undefined {
  if (!capability) return "后端未返回该核心能力";
  const reasons = capability.reason_texts?.length ? capability.reason_texts : capability.reasons;
  return reasons.length > 0 ? reasons.join("；") : undefined;
}

function mergeCoreIDs(registeredCoreIDs: CoreID[], nodes: SmartNode[]): CoreID[] {
  const seen = new Set<CoreID>();
  for (const core of registeredCoreIDs) addCoreID(seen, core);
  for (const node of nodes) {
    addCoreID(seen, node.core_choice);
    addCoreID(seen, node.health?.context.core);
    for (const core of coreIDsForNode(node)) addCoreID(seen, core);
  }
  return [...seen];
}

function mergeCoreIDsForNode(registeredCoreIDs: CoreID[], node: SmartNode): CoreID[] {
  return mergeCoreIDs(registeredCoreIDs, [node]);
}

function addCoreID(seen: Set<CoreID>, core?: CoreID | null) {
  if (core) seen.add(core);
}

function hasActiveFilters(filters: SmartNodeFilters): boolean {
  return Object.values(filters).some(Boolean);
}

function formatTimestamp(value?: string): string {
  if (!value) return "未记录";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("zh-CN", { hour12: false });
}
