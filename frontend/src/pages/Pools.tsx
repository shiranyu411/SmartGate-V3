import { Fragment, useEffect, useMemo, useState } from "react";
import {
  useBusinessPools,
  useBusinessPoolsApply,
  useCores,
  useNodes,
  useProviders,
} from "../lib/queries";
import type {
  BusinessPool,
  BusinessPoolChange,
  PoolRole,
  SmartNode,
} from "../lib/types";
import { Button } from "../ui/Button";
import { Field, Input, Select } from "../ui/Field";
import { Badge } from "../ui/Badge";
import { Card, SectionHeader, Eyebrow } from "../ui/Card";
import { Modal } from "../ui/Modal";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { coreLabel, coreLabelMap, type CoreLabelMap } from "../lib/coreLabels";
import { groupSmartNodesByProvider } from "./nodeState";

const ROLES: PoolRole[] = [
  "general",
  "streaming",
  "ai",
  "banking",
  "crypto",
  "direct",
  "block",
];

const ROLE_LABEL: Record<PoolRole, string> = {
  general: "通用",
  streaming: "流媒体",
  ai: "AI",
  banking: "银行",
  crypto: "加密货币",
  direct: "直连",
  block: "拦截",
};

const SENSITIVE: Set<PoolRole> = new Set(["ai", "banking"]);

export function PoolsPage() {
  const pools = useBusinessPools();
  const nodes = useNodes();
  const cores = useCores();
  const providers = useProviders();
  const apply = useBusinessPoolsApply();
  const [edit, setEdit] = useState<{ pool: BusinessPool; isNew: boolean } | null>(null);
  const [del, setDel] = useState<BusinessPool | null>(null);
  const [error, setError] = useState<unknown>(null);

  const list = pools.data?.business_pools ?? [];
  const allNodes = nodes.data?.nodes ?? [];
  const coreLabels = useMemo(() => coreLabelMap(cores.data), [cores.data]);
  const providerNames = useMemo(
    () => new Map((providers.data?.providers ?? []).map((provider) => [provider.id, provider.name])),
    [providers.data],
  );
  const providerIDs = useMemo(
    () => (providers.data?.providers ?? []).filter((provider) => provider.enabled).map((provider) => provider.id),
    [providers.data],
  );
  const nodeOrder = new Map(allNodes.map((node, index) => [node.id, index]));

  async function submit(change: BusinessPoolChange) {
    setError(null);
    try {
      // Spec-only apply; dry-run plan was discarded without UI, so skip the wasteful call.
      await apply.mutateAsync({ changes: [change] });
      setEdit(null);
      setDel(null);
    } catch (e) {
      setError(e);
    }
  }

  return (
    <div className="space-y-12 max-w-[1120px]">
      <section className="space-y-5">
        <Eyebrow>业务池 · Business Pools</Eyebrow>
        <h1
          className="text-[28px] md:text-[36px] leading-[1.15] font-medium"
          style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.025em" }}
        >
          为不同业务,<br />指派不同的路。
        </h1>
        <p className="max-w-2xl text-[15px] leading-[1.9]" style={{ ...sans, color: "var(--fg)" }}>
          业务池决定哪些节点在哪条业务路径上。AI 与 Banking 是敏感池，没有健康节点时会自动 fail-closed 到 <code style={{ fontFamily: "var(--font-mono)" }}>Block</code>。
        </p>
        <div className="flex flex-wrap items-center gap-3 pt-1">
          <Button onClick={() => setEdit({ pool: newPool(), isNew: true })}>新增业务池</Button>
          {error ? (
            <div className="min-w-0 text-sm" style={{ color: "var(--danger)" }}>
              <ErrorText error={error} fallback="业务池操作失败" showDiagnostic />
            </div>
          ) : null}
        </div>
      </section>

      <SectionHeader
        eyebrow="池清单"
        title={list.length ? `共 ${list.length} 个业务池` : "尚未创建业务池"}
        desc="编辑后直接写入期望配置，由 commit lane 保证一致；数据面需顶栏「立即应用」。"
      />

      <Card>
        {list.length === 0 ? (
          <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>
            暂无业务池。
          </div>
        ) : (
          <ul>
            {list.map((p, i) => (
              <li
                key={p.id}
                className="px-7 py-5 flex items-start gap-5"
                style={{ borderBottom: i === list.length - 1 ? "none" : "1px solid var(--border)" }}
              >
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2 flex-wrap">
                    <p className="text-[18px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                      {p.name || p.id}
                    </p>
                    <Badge tone={SENSITIVE.has(p.role) ? "warn" : "accent"}>
                      {ROLE_LABEL[p.role] || p.role}
                    </Badge>
                    {SENSITIVE.has(p.role) ? <Badge tone="muted">fail-closed</Badge> : null}
                  </div>
                  <p className="text-xs mt-1" style={{ ...sans, color: "var(--muted)" }}>
                    <code style={{ fontFamily: "var(--font-mono)" }}>{p.id}</code> ·
                    {" "}节点 {p.node_ids.length}
                  </p>
                  {p.node_ids.length ? (
                    <>
                      <div className="mt-3 flex flex-wrap gap-1.5">
                        {[...p.node_ids].sort((a, b) => (nodeOrder.get(a) ?? allNodes.length) - (nodeOrder.get(b) ?? allNodes.length)).map((nid) => {
                          const c = classifyNode(allNodes, nid, coreLabels);
                          const bad = c.status !== "ok";
                          return (
                            <span
                              key={nid}
                              title={c.hint}
                              className="text-[11px] px-2 py-0.5 inline-flex items-center gap-1"
                              style={{
                                ...sans,
                                background: bad
                                  ? "color-mix(in srgb, var(--warning) 12%, transparent)"
                                  : "var(--surface-2)",
                                border: `1px solid ${bad ? "color-mix(in srgb, var(--warning) 40%, transparent)" : "var(--border)"}`,
                                color: bad ? "var(--warning)" : "var(--fg-strong)",
                                textDecoration: c.status === "missing" ? "line-through" : undefined,
                              }}
                            >
                              {c.label}
                              {c.status === "excluded" ? " · 已失效" : c.status === "missing" ? " · 已移除" : ""}
                            </span>
                          );
                        })}
                      </div>
                      {(() => {
                        const stale = p.node_ids.filter(
                          (nid) => classifyNode(allNodes, nid, coreLabels).status !== "ok"
                        ).length;
                        return stale ? (
                          <p className="mt-2 text-[11px]" style={{ ...sans, color: "var(--warning)" }}>
                            其中 {stale} 个节点已失效或被移除，建议进入「编辑」清理。
                          </p>
                        ) : null;
                      })()}
                    </>
                  ) : (
                    <p className="mt-3 text-xs italic" style={{ ...serif, color: "var(--muted)" }}>
                      未挂载节点——保存后池可能为空。
                    </p>
                  )}
                </div>
                <div className="flex items-center gap-2">
                  <Button size="sm" variant="ghost" onClick={() => setEdit({ pool: p, isNew: false })}>
                    编辑
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => setDel(p)}>
                    删除
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>

      <PoolEditor
        open={!!edit}
        pool={edit?.pool ?? null}
        isNew={edit?.isNew ?? false}
        nodes={allNodes}
        coreLabels={coreLabels}
        providerNames={providerNames}
        providerIDs={providerIDs}
        loading={apply.isPending}
        onClose={() => setEdit(null)}
        onSubmit={(pool) => submit({ action: "upsert", pool })}
      />

      <Modal
        open={!!del}
        onClose={() => setDel(null)}
        eyebrow="确认删除"
        title={`删除业务池 ${del?.name || del?.id || ""}?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDel(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              loading={apply.isPending}
              onClick={() => del && submit({ action: "delete", id: del.id })}
            >
              删除
            </Button>
          </>
        }
      >
        <p className="text-sm leading-7" style={{ ...sans, color: "var(--fg)" }}>
          删除后会清空对应的代理选择器（敏感池会回退到 Block）。建议先在 Proxy Groups 页核对影响。
        </p>
      </Modal>
    </div>
  );
}

function PoolEditor({
  open,
  pool,
  isNew,
  nodes,
  coreLabels,
  providerNames,
  providerIDs,
  loading,
  onClose,
  onSubmit,
}: {
  open: boolean;
  pool: BusinessPool | null;
  isNew: boolean;
  nodes: SmartNode[];
  coreLabels: CoreLabelMap;
  providerNames: Map<string, string>;
  providerIDs: string[];
  loading: boolean;
  onClose: () => void;
  onSubmit: (pool: BusinessPool) => void;
}) {
  const [form, setForm] = useState<BusinessPool>(pool ?? newPool());
  const [search, setSearch] = useState("");
  const [providerID, setProviderID] = useState("");
  const [onlySelected, setOnlySelected] = useState(false);

  useEffect(() => {
    if (open && pool) {
      setForm(pool);
      setSearch("");
      setProviderID("");
      setOnlySelected(false);
    }
  }, [pool, open]);

  const selected = new Set(form.node_ids);
  const availableIds = new Set(nodes.map((n) => n.id));
  const staleSelected = form.node_ids.filter((id) => !availableIds.has(id));
  const filtered = nodes.filter((n) => {
    if (providerID && !n.aliases.some((alias) => alias.provider_id === providerID)) return false;
    if (onlySelected && !selected.has(n.id)) return false;
    const k = search.trim().toLowerCase();
    if (!k) return true;
    return (
      n.display_name.toLowerCase().includes(k) ||
      n.identity.server.toLowerCase().includes(k) ||
      n.aliases.some((alias) => alias.provider_id.toLowerCase().includes(k) || (providerNames.get(alias.provider_id) ?? "").toLowerCase().includes(k) || alias.raw_tag.toLowerCase().includes(k)) ||
      n.id.toLowerCase().includes(k)
    );
  });
  const groups = groupSmartNodesByProvider(filtered, providerID ? [providerID, ...providerIDs] : providerIDs);
  const filteredSelected = filtered.filter((node) => selected.has(node.id));
  const sourceIDs = [...new Set([...providerIDs, ...nodes.flatMap((node) => node.aliases.map((alias) => alias.provider_id))])];

  function toggle(id: string) {
    setForm((f) => ({
      ...f,
      node_ids: f.node_ids.includes(id)
        ? f.node_ids.filter((x) => x !== id)
        : [...f.node_ids, id],
    }));
  }

  function submit() {
    if (!form.id.trim() || !form.name.trim()) return;
    onSubmit({
      ...form,
      id: form.id.trim(),
      name: form.name.trim(),
    });
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      eyebrow={isNew ? "新增业务池" : "编辑业务池"}
      title={isNew ? "新建池" : `编辑 ${form.name || form.id}`}
      width={760}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button loading={loading} onClick={submit}>
            保存
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Field label="池 ID">
            <Input
              value={form.id}
              disabled={!isNew}
              onChange={(e) => setForm({ ...form, id: e.target.value })}
              placeholder="ai / streaming / ..."
            />
          </Field>
          <Field label="名称">
            <Input
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              placeholder="给用户看的名字"
            />
          </Field>
        </div>
        <Field label="角色 (role)">
          <Select
            value={form.role}
            onChange={(e) => setForm({ ...form, role: e.target.value as PoolRole })}
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {ROLE_LABEL[r]}（{r}）
              </option>
            ))}
          </Select>
        </Field>
        <Field label={`挂载节点（已选 ${form.node_ids.length}）`}>
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="按 tag / server / 订阅源过滤"
          />
        </Field>
        <div className="flex flex-wrap items-center gap-3">
          <div className="min-w-[160px] flex-1">
            <Select aria-label="按 Provider 筛选节点" value={providerID} onChange={(event) => setProviderID(event.target.value)}>
              <option value="">全部 Provider</option>
              {sourceIDs.map((id) => <option key={id} value={id}>{providerNames.get(id) || id}</option>)}
            </Select>
          </div>
          <label className="flex items-center gap-2 text-xs" style={sans}>
            <input type="checkbox" checked={onlySelected} onChange={(event) => setOnlySelected(event.target.checked)} />
            仅看已选
          </label>
          <Button
            size="sm"
            variant="ghost"
            disabled={loading || !filteredSelected.length}
            onClick={() => {
              const remove = new Set(filteredSelected.map((node) => node.id));
              setForm((current) => ({ ...current, node_ids: current.node_ids.filter((id) => !remove.has(id)) }));
            }}
          >
            移除当前筛选（{filteredSelected.length}）
          </Button>
        </div>
        <div
          className="max-h-72 overflow-auto"
          style={{ border: "1px solid var(--border)", background: "var(--surface-2)" }}
        >
          {filtered.length === 0 ? (
            <p className="px-4 py-6 text-xs text-center" style={{ ...sans, color: "var(--muted)" }}>
              没有匹配节点。
            </p>
          ) : (
            <ul>
              {groups.map((group) => (
                <Fragment key={group.providerID}>
                  <li className="px-3 py-2 text-xs font-medium" style={{ ...sans, background: "var(--surface-1)", borderBottom: "1px solid var(--border)" }}>
                    {providerNames.get(group.providerID) || group.providerID || "来源缺失"}
                    <span className="ml-2 tabular-nums" style={{ color: "var(--muted)" }}>
                      {group.nodes.filter((node) => selected.has(node.id)).length} / {group.nodes.length}
                    </span>
                  </li>
                  {group.nodes.map((n) => (
                    <li
                      key={n.id}
                      style={{
                        background: selected.has(n.id) ? "color-mix(in srgb, var(--accent) 10%, transparent)" : "transparent",
                        borderBottom: "1px solid var(--border)",
                      }}
                    >
                      <label className="px-3 py-2 flex items-center gap-3 cursor-pointer">
                        <input
                          type="checkbox"
                          checked={selected.has(n.id)}
                          disabled={loading || (!selected.has(n.id) && (n.source_missing || !n.core_choice || n.capability[n.core_choice]?.status === "unsupported_by_core"))}
                          onChange={() => toggle(n.id)}
                        />
                        <div className="flex-1 min-w-0">
                          <p className="text-sm break-words" style={{ ...sans, color: "var(--fg-strong)" }}>
                            {n.display_name || n.id}
                            <span className="text-xs ml-2" style={{ color: "var(--muted)" }}>
                              {n.identity.protocol}
                              {n.health?.status === "ok" ? ` · ${n.health.delay_ms ?? 0} ms` : n.health?.status === "failed" ? " · 测速失败" : ""}
                            </span>
                          </p>
                          <p className="text-[11px] break-all" style={{ color: "var(--muted)" }}>
                            {n.source_missing ? "来源缺失" : n.aliases.map((alias) => providerNames.get(alias.provider_id) || alias.provider_id).join(", ")}
                            {" · "}{n.core_choice ? coreLabel(n.core_choice, coreLabels) : "核心未设置"}
                            {" · "}{n.identity.server}:{n.identity.port}
                          </p>
                        </div>
                      </label>
                    </li>
                  ))}
                </Fragment>
              ))}
            </ul>
          )}
        </div>
        {staleSelected.length ? (
          <div className="space-y-1.5">
            <p style={eyebrowStyle}>已选但已失效 · {staleSelected.length}（点击移除）</p>
            <div className="flex flex-wrap gap-1.5">
              {staleSelected.map((id) => {
                const n = nodes.find((x) => x.id === id);
                const reason = n?.source_missing ? "来源缺失" : "已从 SmartNode 目录移除";
                return (
                  <button
                    key={id}
                    type="button"
                    onClick={() => toggle(id)}
                    title={`${reason} · 点击从池中移除`}
                    className="text-[11px] px-2 py-1 inline-flex items-center gap-1"
                    style={{
                      ...sans,
                      background: "color-mix(in srgb, var(--warning) 12%, transparent)",
                      border: "1px solid color-mix(in srgb, var(--warning) 40%, transparent)",
                      color: "var(--warning)",
                    }}
                  >
                    {n?.display_name || id} · {reason} · 移除
                  </button>
                );
              })}
            </div>
          </div>
        ) : null}
        {form.node_ids.length === 0 ? (
          <p style={eyebrowStyle}>提示 · 空池将由 fail-closed 策略接管</p>
        ) : null}
      </div>
    </Modal>
  );
}

function newPool(): BusinessPool {
  return {
    id: "",
    name: "",
    role: "general",
    node_ids: [],
    policy: {
      cutover: "graceful",
      graceful_drain_seconds: 30,
      no_candidate: "direct",
      auto_failover: true,
    },
  };
}

function classifyNode(
  nodes: SmartNode[],
  id: string,
  coreLabels: CoreLabelMap
): { label: string; status: "ok" | "excluded" | "missing"; hint?: string } {
  const n = nodes.find((x) => x.id === id);
  if (!n) return { label: id, status: "missing", hint: "该节点已不存在（订阅更新或被删除）" };
  if (n.source_missing) {
    return { label: n.display_name || n.id, status: "excluded", hint: "SmartNode 仍被 Pool 引用，但当前 Provider 快照已无来源" };
  }
  return { label: `${n.display_name || n.id} · ${n.core_choice ? coreLabel(n.core_choice, coreLabels) : "核心未设置"}`, status: "ok" };
}
