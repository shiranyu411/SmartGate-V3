import { useEffect, useMemo, useState } from "react";
import {
  useBusinessPools,
  useCores,
  useExplainRoute,
  useResolveRuleConflict,
  useRules,
  useRulesUpdate,
} from "../lib/queries";
import { makeOpId } from "../lib/api";
import type {
  RouteExplain,
  RouteRule,
  RuleConflict,
  RuleKind,
  RuleSource,
} from "../lib/types";
import { Card, Eyebrow, Rule as Divider, SectionHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Field, Input, Select } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { Segmented, Toggle } from "../ui/Segmented";
import { eyebrowStyle, sans, serif } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { routeDecisionLabel, routeReasonLabel } from "../lib/statusText";
import { coreLabel, coreLabelMap } from "../lib/coreLabels";
import {
  copyRuleAsUser,
  detectRuleConflicts,
  moveConflictPool,
  removeConflictPool,
} from "./ruleState";

type TargetOption = { id: string; label: string };
type SourceFilter = "all" | RuleSource;
type RuleEditorState = { title: string; rule: RouteRule } | null;

const SOURCE_META: Record<RuleSource, { label: string; tone: "muted" | "info" | "accent" }> = {
  "builtin-v2": { label: "内置 V2", tone: "muted" },
  "geo-default": { label: "GEO", tone: "info" },
  user: { label: "用户", tone: "accent" },
};

const KIND_LABEL: Record<RuleKind, string> = {
  domain_suffix: "域名后缀",
  domain_keyword: "域名关键词",
  ip_cidr: "IP / CIDR",
  geosite: "Geosite",
  geoip: "GeoIP",
};

const USER_KINDS: RuleKind[] = [
  "domain_suffix",
  "domain_keyword",
  "ip_cidr",
  "geosite",
  "geoip",
];

export function RoutesPage() {
  const rulesQuery = useRules();
  const poolsQuery = useBusinessPools();
  const saveRules = useRulesUpdate();
  const resolveConflict = useResolveRuleConflict();
  const explain = useExplainRoute();
  const [draft, setDraft] = useState<RouteRule[]>([]);
  const [dirty, setDirty] = useState(false);
  const [filter, setFilter] = useState("");
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>("all");
  const [poolFilter, setPoolFilter] = useState("all");
  const [editor, setEditor] = useState<RuleEditorState>(null);
  const [deleteRule, setDeleteRule] = useState<RouteRule | null>(null);
  const [activeConflict, setActiveConflict] = useState<RuleConflict | null>(null);
  const [pageError, setPageError] = useState<unknown>(null);
  const [target, setTarget] = useState("");
  const [explainResult, setExplainResult] = useState<RouteExplain | null>(null);
  const [explainError, setExplainError] = useState<unknown>(null);

  useEffect(() => {
    if (!rulesQuery.data) return;
    setDraft(rulesQuery.data.rules.map((rule) => ({ ...rule })));
    setDirty(false);
  }, [rulesQuery.data]);

  const targets = useMemo<TargetOption[]>(() => {
    const options: TargetOption[] = [
      { id: "direct", label: "Direct · 直连" },
      { id: "block", label: "Block · 拦截" },
    ];
    for (const pool of poolsQuery.data?.business_pools ?? []) {
      if (options.some((option) => option.id === pool.id)) continue;
      options.push({ id: pool.id, label: `${pool.name || pool.id} · ${pool.id}` });
    }
    return options;
  }, [poolsQuery.data]);

  const conflicts = useMemo(
    () => (dirty ? detectRuleConflicts(draft) : (rulesQuery.data?.conflicts ?? [])),
    [dirty, draft, rulesQuery.data],
  );

  const filtered = useMemo(() => {
    const query = filter.trim().toLowerCase();
    return draft.filter((rule) => {
      if (sourceFilter !== "all" && rule.source !== sourceFilter) return false;
      if (poolFilter !== "all" && rule.pool_id !== poolFilter) return false;
      if (!query) return true;
      return [rule.id, rule.kind, rule.value, rule.pool_id, rule.source]
        .join(" ")
        .toLowerCase()
        .includes(query);
    });
  }, [draft, filter, poolFilter, sourceFilter]);

  function mutateDraft(updater: (current: RouteRule[]) => RouteRule[]) {
    setDraft((current) => updater(current));
    setDirty(true);
    setPageError(null);
  }

  async function persistDraft() {
    setPageError(null);
    try {
      await saveRules.mutateAsync(draft);
      setDirty(false);
    } catch (error) {
      setPageError(error);
    }
  }

  async function onExplain(event: React.FormEvent) {
    event.preventDefault();
    if (!target.trim()) return;
    setExplainError(null);
    try {
      const data = await explain.mutateAsync(target.trim());
      setExplainResult(data);
    } catch (error) {
      setExplainError(error);
      setExplainResult(null);
    }
  }

  const loading = rulesQuery.isLoading || poolsQuery.isLoading;
  const queryError = rulesQuery.error || poolsQuery.error;

  return (
    <div className="space-y-12 max-w-[1120px]">
      <header className="space-y-4">
        <p style={eyebrowStyle}>路由 · Rules</p>
        <h1
          className="text-[28px] md:text-[36px] leading-[1.15] font-medium"
          style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.025em" }}
        >
          每条规则，都有明确归属。
        </h1>
        <p className="text-[15px] leading-[1.85] max-w-3xl" style={{ ...sans, color: "var(--fg)" }}>
          内置 V2 与 GEO 规则保留来源，可单独停用；用户规则可编辑、移动或删除。草稿允许暂存冲突，但配置演练会把未解决冲突列为阻断项。
        </p>
      </header>

      {conflicts.length > 0 ? (
        <ConflictBanner
          count={conflicts.length}
          dirty={dirty}
          onOpen={() => setActiveConflict(conflicts[0] ?? null)}
        />
      ) : null}

      <Divider />

      <section className="space-y-6">
        <SectionHeader
          eyebrow="统一规则目录"
          title={loading ? "载入规则…" : `共 ${draft.length} 条规则`}
          desc="保存写入期望配置；真正进入 Gateway Fabric 仍需顶栏「立即应用」。"
          right={
            <div className="flex items-center gap-2 flex-wrap">
              {dirty ? <Badge tone="warn">草稿未保存</Badge> : <Badge tone="muted">已与后端同步</Badge>}
              <Button
                size="sm"
                onClick={() =>
                  setEditor({
                    title: "新增用户规则",
                    rule: newUserRule(targets[0]?.id ?? "direct"),
                  })
                }
                disabled={loading}
              >
                新增规则
              </Button>
              <Button size="sm" onClick={persistDraft} loading={saveRules.isPending} disabled={!dirty}>
                保存草稿
              </Button>
              <Button
                size="sm"
                variant="ghost"
                disabled={!dirty || !rulesQuery.data}
                onClick={() => {
                  setDraft((rulesQuery.data?.rules ?? []).map((rule) => ({ ...rule })));
                  setDirty(false);
                  setPageError("");
                }}
              >
                放弃修改
              </Button>
            </div>
          }
        />

        <div className="grid md:grid-cols-3 gap-3">
          <Input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder="搜索规则内容 / ID / Pool"
            aria-label="搜索规则"
          />
          <Select
            value={sourceFilter}
            onChange={(event) => setSourceFilter(event.target.value as SourceFilter)}
            aria-label="按来源筛选"
          >
            <option value="all">全部来源</option>
            <option value="builtin-v2">内置 V2</option>
            <option value="geo-default">GEO</option>
            <option value="user">用户</option>
          </Select>
          <Select value={poolFilter} onChange={(event) => setPoolFilter(event.target.value)} aria-label="按 Pool 筛选">
            <option value="all">全部 Pool</option>
            {targets.map((option) => (
              <option key={option.id} value={option.id}>{option.label}</option>
            ))}
          </Select>
        </div>

        {pageError || queryError ? (
          <Card>
            <div className="px-7 py-5 text-sm break-words" style={{ ...sans, color: "var(--danger)" }}>
              <ErrorText error={pageError || queryError} fallback="路由规则加载失败" showDiagnostic />
            </div>
          </Card>
        ) : null}

        {loading ? (
          <Card>
            <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>载入中…</div>
          </Card>
        ) : filtered.length === 0 ? (
          <Card>
            <div className="px-7 py-12 text-center" style={{ ...sans, color: "var(--muted)" }}>没有匹配的规则。</div>
          </Card>
        ) : (
          <RulesTable
            rules={filtered}
            targets={targets}
            onToggle={(rule, enabled) =>
              mutateDraft((current) => current.map((item) => item.id === rule.id ? { ...item, enabled } : item))
            }
            onEdit={(rule) => setEditor({ title: "编辑用户规则", rule: { ...rule } })}
            onCopy={(rule) =>
              setEditor({
                title: "复制为用户规则",
                rule: copyRuleAsUser(rule, makeOpId("user-rule")),
              })
            }
            onDelete={setDeleteRule}
          />
        )}
      </section>

      <Divider glyph="§" />

      <section className="space-y-6">
        <SectionHeader
          eyebrow="路由演算"
          title="解释一次完整决策"
          desc="显示命中规则、Pool、当前 Binding、执行核心 / Worker 与 DNS 路径。"
        />
        <form
          onSubmit={onExplain}
          className="flex flex-wrap items-end gap-3 px-7 py-6"
          style={{
            background: "var(--surface)",
            borderTop: "1px solid var(--border-strong)",
            borderBottom: "1px solid var(--border-strong)",
          }}
        >
          <div className="flex-1 min-w-[280px]">
            <Field label="目标" hint="支持域名 / URL / IPv4 / IPv6">
              <Input
                value={target}
                onChange={(event) => setTarget(event.target.value)}
                placeholder="例如 https://chatgpt.com/codex"
              />
            </Field>
          </div>
          <Button type="submit" loading={explain.isPending} disabled={!target.trim()}>解释</Button>
        </form>

        {explainError ? (
          <p className="text-sm" style={{ ...sans, color: "var(--danger)" }}>
            <ErrorText error={explainError} fallback="路由解释失败" showDiagnostic />
          </p>
        ) : null}
        {explainResult ? <ExplainCard result={explainResult} /> : null}
      </section>

      <RuleEditModal
        state={editor}
        targets={targets}
        onClose={() => setEditor(null)}
        onSubmit={(rule) => {
          mutateDraft((current) => {
            const index = current.findIndex((item) => item.id === rule.id);
            if (index < 0) return [...current, rule];
            return current.map((item) => item.id === rule.id ? rule : item);
          });
          setEditor(null);
        }}
      />

      <DeleteRuleModal
        rule={deleteRule}
        onClose={() => setDeleteRule(null)}
        onConfirm={() => {
          if (!deleteRule) return;
          mutateDraft((current) => current.filter((rule) => rule.id !== deleteRule.id));
          setDeleteRule(null);
        }}
      />

      <ConflictModal
        conflict={activeConflict}
        dirty={dirty}
        targets={targets}
        loading={resolveConflict.isPending}
        error={resolveConflict.error}
        onClose={() => setActiveConflict(null)}
        onKeep={async (keepPool) => {
          if (!activeConflict) return;
          await resolveConflict.mutateAsync({ ruleKey: activeConflict.rule_key, keepPool });
          setActiveConflict(null);
        }}
        onRemove={(pool) => {
          if (!activeConflict) return;
          mutateDraft((current) => removeConflictPool(current, activeConflict, pool));
          setActiveConflict(null);
        }}
        onMove={(fromPool, toPool) => {
          if (!activeConflict) return;
          mutateDraft((current) => moveConflictPool(current, activeConflict, fromPool, toPool));
          setActiveConflict(null);
        }}
      />

      <Eyebrow>SmartGate · 书房 · 第三期 · 规则与解释</Eyebrow>
    </div>
  );
}

function ConflictBanner({ count, dirty, onOpen }: { count: number; dirty: boolean; onOpen: () => void }) {
  return (
    <Card
      style={{
        borderColor: "color-mix(in srgb, var(--warning) 45%, var(--border))",
        background: "color-mix(in srgb, var(--warning) 8%, var(--surface))",
      }}
    >
      <div className="px-6 py-5 flex items-center justify-between gap-4 flex-wrap">
        <div>
          <div className="flex items-center gap-2 flex-wrap">
            <Badge tone="warn">阻断 Apply</Badge>
            <p className="text-[17px]" style={{ ...serif, color: "var(--fg-strong)" }}>
              {count} 条规则存在归属冲突
            </p>
          </div>
          <p className="text-sm mt-1.5" style={{ ...sans, color: "var(--fg)" }}>
            {dirty ? "当前是未保存草稿；可先移除或移动冲突项，再保存。" : "可以保留一个最终 Pool，或返回目录继续编辑。"}
          </p>
        </div>
        <Button variant="ghost" onClick={onOpen}>打开冲突面板</Button>
      </div>
    </Card>
  );
}

function RulesTable({
  rules,
  targets,
  onToggle,
  onEdit,
  onCopy,
  onDelete,
}: {
  rules: RouteRule[];
  targets: TargetOption[];
  onToggle: (rule: RouteRule, enabled: boolean) => void;
  onEdit: (rule: RouteRule) => void;
  onCopy: (rule: RouteRule) => void;
  onDelete: (rule: RouteRule) => void;
}) {
  return (
    <Card>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[880px] text-sm" style={{ ...sans, color: "var(--fg)" }}>
          <thead>
            <tr style={{ borderBottom: "1px solid var(--border-strong)" }}>
              <Th>规则</Th>
              <Th>来源</Th>
              <Th>归属 Pool</Th>
              <Th>启用</Th>
              <Th align="right">操作</Th>
            </tr>
          </thead>
          <tbody>
            {rules.map((rule) => {
              const source = SOURCE_META[rule.source];
              return (
                <tr
                  key={rule.id}
                  style={{
                    borderBottom: "1px solid var(--border)",
                    opacity: rule.overridden_by ? 0.58 : 1,
                  }}
                >
                  <Td>
                    <div className="flex items-center gap-2 flex-wrap">
                      <code style={{ fontFamily: "var(--font-mono)", color: "var(--fg-strong)" }}>{rule.value}</code>
                      <Badge tone="muted">{KIND_LABEL[rule.kind]}</Badge>
                    </div>
                    <p className="text-[11px] mt-1 break-all" style={{ color: "var(--muted)" }}>{rule.id}</p>
                    {rule.overridden_by ? (
                      <p className="text-xs mt-1" style={{ color: "var(--muted)" }}>
                        已被用户规则 <code style={{ fontFamily: "var(--font-mono)" }}>{rule.overridden_by}</code> 覆盖
                      </p>
                    ) : null}
                  </Td>
                  <Td><Badge tone={source.tone}>{source.label}</Badge></Td>
                  <Td>
                    <span style={{ color: "var(--fg-strong)" }}>{targetLabel(rule.pool_id, targets)}</span>
                    <code className="block text-[11px] mt-1" style={{ fontFamily: "var(--font-mono)", color: "var(--muted)" }}>
                      {rule.pool_id}
                    </code>
                  </Td>
                  <Td>
                    <div className="flex items-center gap-2">
                      <Toggle on={rule.enabled} onChange={(enabled) => onToggle(rule, enabled)} />
                      <span className="text-xs" style={{ color: "var(--muted)" }}>{rule.enabled ? "启用" : "停用"}</span>
                    </div>
                  </Td>
                  <Td align="right">
                    <div className="flex items-center justify-end gap-2">
                      {rule.source === "user" ? (
                        <>
                          <Button size="sm" variant="ghost" onClick={() => onEdit(rule)}>编辑</Button>
                          <Button size="sm" variant="danger" onClick={() => onDelete(rule)}>删除</Button>
                        </>
                      ) : (
                        <Button size="sm" variant="ghost" onClick={() => onCopy(rule)}>复制为用户规则</Button>
                      )}
                    </div>
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function RuleEditModal({
  state,
  targets,
  onClose,
  onSubmit,
}: {
  state: RuleEditorState;
  targets: TargetOption[];
  onClose: () => void;
  onSubmit: (rule: RouteRule) => void;
}) {
  const [form, setForm] = useState<RouteRule | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    setForm(state ? { ...state.rule } : null);
    setError("");
  }, [state]);

  if (!state || !form) return null;

  return (
    <Modal
      open
      onClose={onClose}
      eyebrow="用户规则"
      title={state.title}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button
            onClick={() => {
              if (!form.value.trim()) {
                setError("规则内容不能为空");
                return;
              }
              onSubmit({ ...form, value: form.value.trim(), source: "user", overridden_by: undefined });
            }}
          >
            写入草稿
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <Field label="匹配类型">
          <Select value={form.kind} onChange={(event) => setForm({ ...form, kind: event.target.value as RuleKind })}>
            {USER_KINDS.map((kind) => <option key={kind} value={kind}>{KIND_LABEL[kind]}</option>)}
          </Select>
        </Field>
        <Field label="规则内容" error={error} hint={ruleHint(form.kind)}>
          <Input value={form.value} onChange={(event) => setForm({ ...form, value: event.target.value })} autoFocus />
        </Field>
        <Field label="归属 Pool">
          <Select value={form.pool_id} onChange={(event) => setForm({ ...form, pool_id: event.target.value })}>
            {targets.map((option) => <option key={option.id} value={option.id}>{option.label}</option>)}
          </Select>
        </Field>
        <div className="flex items-center justify-between gap-4 px-4 py-3" style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}>
          <div>
            <p style={{ ...serif, color: "var(--fg-strong)" }}>启用规则</p>
            <p className="text-xs mt-1" style={{ color: "var(--muted)" }}>停用规则仍保留在草稿中。</p>
          </div>
          <Toggle on={form.enabled} onChange={(enabled) => setForm({ ...form, enabled })} />
        </div>
      </div>
    </Modal>
  );
}

function DeleteRuleModal({ rule, onClose, onConfirm }: { rule: RouteRule | null; onClose: () => void; onConfirm: () => void }) {
  return (
    <Modal
      open={Boolean(rule)}
      onClose={onClose}
      eyebrow="删除用户规则"
      title="从草稿中移除这条规则？"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button variant="danger" onClick={onConfirm}>移除</Button>
        </>
      }
    >
      <p className="text-[15px] leading-7" style={{ ...serif, color: "var(--fg)" }}>
        <code style={{ fontFamily: "var(--font-mono)", color: "var(--fg-strong)" }}>{rule?.value}</code>
        {" "}只会从当前草稿移除；点击「保存草稿」后才会写入后端。
      </p>
    </Modal>
  );
}

function ConflictModal({
  conflict,
  dirty,
  targets,
  loading,
  error,
  onClose,
  onKeep,
  onRemove,
  onMove,
}: {
  conflict: RuleConflict | null;
  dirty: boolean;
  targets: TargetOption[];
  loading: boolean;
  error: unknown;
  onClose: () => void;
  onKeep: (pool: string) => Promise<void>;
  onRemove: (pool: string) => void;
  onMove: (fromPool: string, toPool: string) => void;
}) {
  const [keepPool, setKeepPool] = useState("");
  const [removePool, setRemovePool] = useState("");
  const [moveFrom, setMoveFrom] = useState("");
  const [moveTo, setMoveTo] = useState("");

  useEffect(() => {
    const first = conflict?.pools[0] ?? "";
    setKeepPool(first);
    setRemovePool(first);
    setMoveFrom(first);
    setMoveTo(targets.find((target) => target.id !== first)?.id ?? first);
  }, [conflict, targets]);

  if (!conflict) return null;

  return (
    <Modal
      open
      onClose={onClose}
      eyebrow="规则冲突消歧"
      title={conflict.value}
      width={760}
      footer={<Button variant="ghost" onClick={onClose}>返回编辑</Button>}
    >
      <div className="space-y-6">
        <div className="flex items-center gap-2 flex-wrap">
          <Badge tone="warn">{KIND_LABEL[conflict.kind]}</Badge>
          {conflict.pools.map((pool) => <Badge key={pool} tone="muted">{targetLabel(pool, targets)}</Badge>)}
        </div>

        <ConflictOperation title="保留最终归属" desc="立即调用消歧端点：保留所选 Pool，并删除其余冲突用户规则。">
          <div className="flex items-center justify-between gap-3 flex-wrap">
            <Segmented
              value={keepPool}
              onChange={setKeepPool}
              options={conflict.pools.map((pool) => ({ key: pool, label: targetLabel(pool, targets) }))}
              disabled={dirty || loading}
            />
            <Button
              size="sm"
              loading={loading}
              disabled={dirty || !keepPool}
              onClick={() => void onKeep(keepPool)}
            >
              保留此 Pool
            </Button>
          </div>
          {dirty ? <p className="text-xs mt-2" style={{ color: "var(--warning)" }}>请先保存或放弃当前草稿，再执行服务端一键保留。</p> : null}
        </ConflictOperation>

        <ConflictOperation title="从某 Pool 移除" desc="只修改当前草稿，移除该 Pool 对此规则的用户归属。">
          <div className="flex items-center gap-3 flex-wrap">
            <Select className="max-w-xs" value={removePool} onChange={(event) => setRemovePool(event.target.value)}>
              {conflict.pools.map((pool) => <option key={pool} value={pool}>{targetLabel(pool, targets)}</option>)}
            </Select>
            <Button size="sm" variant="danger" onClick={() => onRemove(removePool)} disabled={!removePool}>从该 Pool 移除</Button>
          </div>
        </ConflictOperation>

        <ConflictOperation title="移动到另一 Pool" desc="把一个冲突归属移动到新目标；保存后由后端重新计算冲突。">
          <div className="grid sm:grid-cols-[1fr_auto_1fr_auto] gap-3 items-center">
            <Select value={moveFrom} onChange={(event) => setMoveFrom(event.target.value)}>
              {conflict.pools.map((pool) => <option key={pool} value={pool}>{targetLabel(pool, targets)}</option>)}
            </Select>
            <span className="text-center" style={{ color: "var(--muted)" }}>→</span>
            <Select value={moveTo} onChange={(event) => setMoveTo(event.target.value)}>
              {targets.map((target) => <option key={target.id} value={target.id}>{target.label}</option>)}
            </Select>
            <Button size="sm" variant="ghost" onClick={() => onMove(moveFrom, moveTo)} disabled={!moveFrom || !moveTo || moveFrom === moveTo}>移动</Button>
          </div>
        </ConflictOperation>

        {error ? (
          <p className="text-sm break-words" style={{ color: "var(--danger)" }}>
            <ErrorText error={error} fallback="规则冲突处理失败" showDiagnostic />
          </p>
        ) : null}
      </div>
    </Modal>
  );
}

function ConflictOperation({ title, desc, children }: { title: string; desc: string; children: React.ReactNode }) {
  return (
    <section className="px-5 py-4 space-y-3" style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}>
      <div>
        <h3 className="text-[17px]" style={{ ...serif, color: "var(--fg-strong)" }}>{title}</h3>
        <p className="text-xs mt-1" style={{ color: "var(--muted)" }}>{desc}</p>
      </div>
      {children}
    </section>
  );
}

function ExplainCard({ result }: { result: RouteExplain }) {
  const cores = useCores();
  const coreLabels = useMemo(() => coreLabelMap(cores.data), [cores.data]);
  const matched = result.matched_rule;
  const binding = result.binding;
  const dnsPath = result.dns_path ?? [];
  const chain = result.chain ?? [];
  return (
    <Card>
      <div className="px-7 py-6 space-y-5" style={{ ...sans, color: "var(--fg)" }}>
        <div className="flex items-center flex-wrap gap-3">
          <code style={{ fontFamily: "var(--font-mono)", color: "var(--fg-strong)" }}>{result.target}</code>
          <span title={result.decision}><Badge tone="accent">{routeDecisionLabel(result.decision)}</Badge></span>
          {result.pool || result.pool_id ? <Badge tone="info">Pool · {result.pool || result.pool_id}</Badge> : null}
        </div>

        <div className="grid md:grid-cols-3 gap-3">
          <ExplainBlock title="命中规则">
            {matched ? (
              <>
                <code className="break-all" style={{ fontFamily: "var(--font-mono)" }}>{matched.value}</code>
                <div className="flex items-center gap-1.5 flex-wrap mt-2">
                  <Badge tone={SOURCE_META[matched.source].tone}>{SOURCE_META[matched.source].label}</Badge>
                  <Badge tone="muted">{KIND_LABEL[matched.kind]}</Badge>
                </div>
              </>
            ) : <span>{result.matched || "未命中显式规则"}</span>}
          </ExplainBlock>
          <ExplainBlock title="当前 Binding">
            {binding ? (
              <div className="space-y-1">
                <p>{binding.smart_node || binding.target || "—"}</p>
                <p className="text-xs" style={{ color: "var(--muted)" }}>
                  Generation {binding.generation ?? "—"} · {binding.core ? coreLabel(binding.core, coreLabels) : "无核心"}
                </p>
                <p className="text-xs" style={{ color: "var(--muted)" }}>{binding.worker || "无 Worker"}</p>
              </div>
            ) : <span>尚无 Applied Binding</span>}
          </ExplainBlock>
          <ExplainBlock title="原因"><span title={result.reason}>{routeReasonLabel(result.reason)}</span></ExplainBlock>
        </div>

        <PathList title="DNS 路径" items={dnsPath} empty="当前响应未附 DNS 路径" tone="info" />
        <PathList title="规则链" items={chain} empty="当前响应未附规则链" tone="muted" />
      </div>
    </Card>
  );
}

function ExplainBlock({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="px-4 py-3" style={{ background: "var(--surface-2)", border: "1px solid var(--border)" }}>
      <p style={eyebrowStyle}>{title}</p>
      <div className="mt-2 text-sm" style={{ color: "var(--fg-strong)" }}>{children}</div>
    </div>
  );
}

function PathList({
  title,
  items,
  empty,
  tone,
}: {
  title: string;
  items: string[];
  empty: string;
  tone: "info" | "muted";
}) {
  return (
    <div>
      <p style={eyebrowStyle}>{title}</p>
      <ol className="mt-2 flex flex-wrap gap-2">
        {items.length === 0 ? (
          <li className="text-xs" style={{ color: "var(--muted)" }}>{empty}</li>
        ) : items.map((item, index) => (
          <li key={`${item}-${index}`}><Badge tone={tone}>{index + 1}. {item}</Badge></li>
        ))}
      </ol>
    </div>
  );
}

function Th({ children, align = "left" }: { children: React.ReactNode; align?: "left" | "right" }) {
  return (
    <th
      className="px-6 py-3 text-[11px]"
      style={{
        color: "var(--muted)",
        textAlign: align,
        textTransform: "uppercase",
        letterSpacing: "0.16em",
      }}
    >
      {children}
    </th>
  );
}

function Td({ children, align = "left" }: { children: React.ReactNode; align?: "left" | "right" }) {
  return <td className="px-6 py-4 align-top" style={{ textAlign: align }}>{children}</td>;
}

function newUserRule(poolID: string): RouteRule {
  return {
    id: makeOpId("user-rule"),
    source: "user",
    kind: "domain_suffix",
    value: "",
    pool_id: poolID,
    enabled: true,
  };
}

function targetLabel(poolID: string, targets: TargetOption[]): string {
  return targets.find((target) => target.id === poolID)?.label ?? poolID;
}

function ruleHint(kind: RuleKind): string {
  switch (kind) {
    case "domain_suffix": return "例如 openai.com";
    case "domain_keyword": return "例如 chatgpt";
    case "ip_cidr": return "例如 203.0.113.0/24";
    case "geosite": return "例如 geosite-cn";
    case "geoip": return "例如 geoip-cn";
  }
}
