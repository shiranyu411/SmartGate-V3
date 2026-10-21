import { useEffect, useState } from "react";
import { useRuleSets, useRuleSetsUpdate } from "../lib/queries";
import type {
  RemoteRuleSettings,
  RemoteRuleSet,
  RemoteRuleTarget,
  RemoteRuleFormat,
} from "../lib/types";
import { Card, SectionHeader, Rule, Eyebrow } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Modal } from "../ui/Modal";
import { Field, Input, Select } from "../ui/Field";
import { Toggle } from "../ui/Segmented";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { ruleFormatLabel } from "../lib/statusText";

export function RuleSetsPage() {
  const query = useRuleSets();
  const update = useRuleSetsUpdate();
  const [draft, setDraft] = useState<RemoteRuleSettings | null>(null);
  const [editSet, setEditSet] = useState<{ mode: "create" } | { mode: "edit"; set: RemoteRuleSet } | null>(null);
  const [deleteId, setDeleteId] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (query.data && !draft) {
      setDraft(structuredClone(query.data));
    }
  }, [query.data, draft]);

  function resetDraft() {
    if (query.data) setDraft(structuredClone(query.data));
  }

  const dirty = draft && query.data ? JSON.stringify(draft) !== JSON.stringify(query.data) : false;

  async function save() {
    if (!draft) return;
    setError(null);
    try {
      const saved = await update.mutateAsync(draft);
      // Sync draft to server-normalized response so display-name defaults
      // (and other normalize fills) do not leave a permanent fake dirty state.
      setDraft(structuredClone(saved));
    } catch (e) {
      setError(e);
    }
  }

  function toggleGlobal(enabled: boolean) {
    if (!draft) return;
    setDraft({ ...draft, enabled });
  }

  function toggleAdBlock(enabled: boolean) {
    if (!draft) return;
    setDraft({ ...draft, ad_block: { ...draft.ad_block, enabled } });
  }

  function toggleSet(id: string, enabled: boolean) {
    if (!draft) return;
    setDraft({
      ...draft,
      sets: draft.sets.map((s) => (s.id === id ? { ...s, enabled } : s)),
    });
  }

  function deleteSet(id: string) {
    if (!draft) return;
    setDraft({
      ...draft,
      sets: draft.sets.filter((s) => s.id !== id),
      ad_block: {
        ...draft.ad_block,
        sets: draft.ad_block.sets.filter((s) => s !== id),
      },
    });
    setDeleteId(null);
  }

  function upsertSet(set: RemoteRuleSet) {
    if (!draft) return;
    const exists = draft.sets.find((s) => s.id === set.id);
    if (exists) {
      setDraft({ ...draft, sets: draft.sets.map((s) => (s.id === set.id ? set : s)) });
    } else {
      setDraft({ ...draft, sets: [...draft.sets, set] });
    }
    setEditSet(null);
  }

  if (query.isLoading || !draft) {
    return (
      <div className="space-y-12 max-w-[1120px]">
        <p className="text-sm" style={{ ...sans, color: "var(--muted)" }}>载入中…</p>
      </div>
    );
  }

  if (query.error) {
    return (
      <div className="space-y-12 max-w-[1120px]">
        <p className="text-sm" style={{ ...sans, color: "var(--danger)" }}>
          <ErrorText error={query.error} fallback="远程规则加载失败" />
        </p>
      </div>
    );
  }

  const enabledCount = draft.sets.filter((s) => s.enabled).length;
  const totalBytes = draft.sets
    .filter((s) => s.enabled)
    .reduce((sum, s) => sum + (s.estimated_bytes || 0), 0);
  const guard = draft.memory_guard;

  return (
    <div className="space-y-12 max-w-[1120px]">
      <header className="space-y-3">
        <p style={eyebrowStyle}>远程规则 · Rule Sets</p>
        <h1
          className="text-[28px] md:text-[36px] leading-[1.15] font-medium"
          style={{ ...serif, color: "var(--fg-strong)", letterSpacing: "-0.025em" }}
        >
          远程规则源管理
        </h1>
        <p className="text-[15px] leading-[1.85] max-w-2xl" style={{ ...sans, color: "var(--fg)" }}>
          数据平面直接拉取远程规则集，不占用 SmartGate 进程内存。开启的规则集会在后台下载并定期更新。
        </p>
      </header>

      <Rule />

      {/* 全局开关 + 广告拦截 */}
      <section className="space-y-6">
        <SectionHeader eyebrow="全局" title="功能开关" />
        <Card>
          <div className="px-7 py-5 space-y-5">
            <ToggleRow
              label="远程规则总开关"
              hint="关闭后所有远程规则集不写入数据平面配置"
              on={draft.enabled}
              onChange={toggleGlobal}
            />
            <div style={{ borderTop: "1px solid var(--border)" }} />
            <ToggleRow
              label="广告拦截"
              hint="启用后关联的广告规则集将生效（默认 geosite-category-ads）"
              on={draft.ad_block.enabled}
              onChange={toggleAdBlock}
              disabled={!draft.enabled}
            />
          </div>
        </Card>
      </section>

      {/* 内存警戒 */}
      <section className="space-y-6">
        <SectionHeader eyebrow="资源" title="内存警戒" desc="路由器内存有限，过多或过大的规则集可能影响稳定性。" />
        <Card>
          <div className="px-7 py-5">
            <div className="grid grid-cols-2 lg:grid-cols-4 gap-6">
              <MemoryGauge
                label="已启用"
                value={enabledCount}
                warn={guard.warn_enabled_sets}
                max={guard.max_enabled_sets}
                unit="个"
              />
              <MemoryGauge
                label="预估内存"
                value={Math.round(totalBytes / 1024)}
                warn={Math.round(guard.warn_total_bytes / 1024)}
                max={Math.round(guard.max_total_bytes / 1024)}
                unit="KB"
              />
              <div className="text-sm space-y-1" style={{ color: "var(--muted)" }}>
                <p>单集上限</p>
                <p style={{ color: "var(--fg-strong)" }}>{Math.round(guard.max_set_bytes / 1024)} KB</p>
              </div>
              <div className="text-sm space-y-1" style={{ color: "var(--muted)" }}>
                <p>更新间隔</p>
                <p style={{ color: "var(--fg-strong)" }}>{draft.update_interval}</p>
              </div>
            </div>
          </div>
        </Card>
      </section>

      {/* 警告 */}
      {draft.warnings && draft.warnings.length > 0 ? (
        <div
          className="px-5 py-3"
          style={{
            background: "color-mix(in srgb, var(--warning) 10%, transparent)",
            border: "1px solid color-mix(in srgb, var(--warning) 35%, transparent)",
            borderRadius: "var(--radius-md)",
          }}
        >
          {draft.warnings.map((w, i) => (
            <p key={i} className="text-sm" style={{ ...sans, color: "var(--warning)" }}>{w}</p>
          ))}
        </div>
      ) : null}

      {/* 规则列表 */}
      <section className="space-y-6">
        <SectionHeader
          eyebrow="清单"
          title="规则集列表"
          right={
            <Button size="sm" onClick={() => setEditSet({ mode: "create" })} disabled={!draft.enabled}>
              新增规则集
            </Button>
          }
        />
        <Card>
          {draft.sets.length === 0 ? (
            <div className="px-7 py-10 text-center">
              <p className="text-sm" style={{ ...sans, color: "var(--muted)" }}>暂无规则集，点右上角新增。</p>
            </div>
          ) : (
            <div className="overflow-x-auto">
            <table className="w-full text-sm min-w-[720px]" style={sans}>
              <thead>
                <tr style={{ color: "var(--muted)", borderBottom: "1px solid var(--border)" }}>
                  <Th>名称</Th>
                  <Th>类型</Th>
                  <Th>目标</Th>
                  <Th>格式</Th>
                  <Th>预估大小</Th>
                  <Th>启用</Th>
                  <Th align="right">操作</Th>
                </tr>
              </thead>
              <tbody>
                {draft.sets.map((s, i) => (
                  <tr
                    key={s.id}
                    style={{ borderBottom: i === draft.sets.length - 1 ? "none" : "1px solid var(--border)" }}
                  >
                    <td className="px-6 py-4">
                      <p className="text-[14px]" style={{ color: "var(--fg-strong)" }}>{s.name || s.id}</p>
                      <p className="text-[11px] mt-0.5" style={{ color: "var(--muted)", fontFamily: "var(--font-mono)" }}>{s.id}</p>
                    </td>
                    <td className="px-3 py-4">
                      <Badge tone={kindTone(s.kind)}>{kindLabel(s.kind)}</Badge>
                    </td>
                    <td className="px-3 py-4">
                      <Badge tone={targetTone(s.target)}>{targetLabel(s.target)}</Badge>
                    </td>
                    <td className="px-3 py-4">
                      <span className="text-xs" style={{ color: "var(--muted)" }} title={s.format || "binary"}>{ruleFormatLabel(s.format)}</span>
                    </td>
                    <td className="px-3 py-4 tabular-nums">
                      <span style={{ color: "var(--fg)" }}>{s.estimated_bytes ? `${Math.round(s.estimated_bytes / 1024)} KB` : "—"}</span>
                    </td>
                    <td className="px-3 py-4">
                      <Toggle
                        on={s.enabled}
                        onChange={(v) => toggleSet(s.id, v)}
                        disabled={!draft.enabled}
                      />
                    </td>
                    <td className="px-6 py-4">
                      <div className="flex items-center justify-end gap-1.5">
                        <Button size="sm" variant="ghost" onClick={() => setEditSet({ mode: "edit", set: s })}>
                          编辑
                        </Button>
                        <Button size="sm" variant="ghost" onClick={() => setDeleteId(s.id)}>
                          删除
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          )}
        </Card>
      </section>

      {/* 保存栏 */}
      {error ? (
        <p className="break-words text-sm" style={{ ...sans, color: "var(--danger)" }}>
          <ErrorText error={error} fallback="保存远程规则失败" showDiagnostic />
        </p>
      ) : null}
      <div className="flex items-center gap-4">
        <Button onClick={save} loading={update.isPending} disabled={!dirty}>
          保存到期望
        </Button>
        <Button variant="ghost" onClick={resetDraft} disabled={!dirty || update.isPending}>
          还原
        </Button>
        {dirty ? <Badge tone="warn">有未保存的更改</Badge> : <Badge tone="muted">与后端一致</Badge>}
        <span className="text-xs ml-auto" style={{ ...sans, color: "var(--muted)" }}>
          保存后请在顶栏点"立即应用"进入演练。
        </span>
      </div>

      {/* 编辑 Modal */}
      {editSet ? (
        <RuleSetEditModal
          initial={editSet.mode === "edit" ? editSet.set : undefined}
          settings={draft}
          onClose={() => setEditSet(null)}
          onSubmit={upsertSet}
        />
      ) : null}

      {/* 删除确认 */}
      {deleteId ? (
        <Modal
          open
          onClose={() => setDeleteId(null)}
          eyebrow="删除规则集"
          title={`确认删除 ${deleteId}？`}
          width={480}
          footer={
            <>
              <Button variant="ghost" onClick={() => setDeleteId(null)}>取消</Button>
              <Button variant="danger" onClick={() => deleteSet(deleteId)}>确认删除</Button>
            </>
          }
        >
          <p className="text-sm" style={{ ...sans, color: "var(--fg)" }}>
            删除后该规则集将不再写入数据平面配置。需要保存并应用后才真正生效。
          </p>
        </Modal>
      ) : null}

      <Eyebrow>
        <span style={{ letterSpacing: "0.3em" }}>SmartGate · 书房 · 远程规则</span>
      </Eyebrow>
    </div>
  );
}

// ===== 子组件 =====

function ToggleRow({
  label,
  hint,
  on,
  onChange,
  disabled,
}: {
  label: string;
  hint: string;
  on: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <p className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>{label}</p>
        <p className="text-xs mt-0.5" style={{ ...sans, color: "var(--muted)" }}>{hint}</p>
      </div>
      <Toggle on={on} onChange={onChange} disabled={disabled} />
    </div>
  );
}

function MemoryGauge({
  label,
  value,
  warn,
  max,
  unit,
}: {
  label: string;
  value: number;
  warn: number;
  max: number;
  unit: string;
}) {
  const pct = Math.min((value / max) * 100, 100);
  const tone = value >= max ? "var(--danger)" : value >= warn ? "var(--warning)" : "var(--success)";
  return (
    <div className="space-y-2">
      <p className="text-xs" style={{ ...sans, color: "var(--muted)" }}>{label}</p>
      <p className="text-[20px] font-medium tabular-nums" style={{ color: "var(--fg-strong)" }}>
        {value} <span className="text-xs font-normal" style={{ color: "var(--muted)" }}>/ {max} {unit}</span>
      </p>
      <div className="h-1.5 rounded-full overflow-hidden" style={{ background: "var(--surface-2)" }}>
        <div
          className="h-full rounded-full transition-all duration-300"
          style={{ width: `${pct}%`, background: tone }}
        />
      </div>
    </div>
  );
}

function Th({ children, align }: { children: React.ReactNode; align?: "right" }) {
  return (
    <th
      className="px-6 py-3 text-[11px] font-medium"
      style={{ ...eyebrowStyle, textAlign: align || "left" }}
    >
      {children}
    </th>
  );
}

// ===== 编辑 Modal =====

function RuleSetEditModal({
  initial,
  settings,
  onClose,
  onSubmit,
}: {
  initial?: RemoteRuleSet;
  settings: RemoteRuleSettings;
  onClose: () => void;
  onSubmit: (set: RemoteRuleSet) => void;
}) {
  const isEdit = Boolean(initial);
  const [form, setForm] = useState<RemoteRuleSet>(
    () =>
      initial ?? {
        id: "",
        name: "",
        enabled: true,
        kind: "custom",
        target: "proxy",
        format: settings.default_format,
        type: "remote",
        url: "",
        download_detour: settings.download_detour,
        update_interval: settings.update_interval,
        estimated_bytes: 0,
        tags: [],
      },
  );

  function update<K extends keyof RemoteRuleSet>(k: K, v: RemoteRuleSet[K]) {
    setForm((s) => ({ ...s, [k]: v }));
  }

  const idValid = /^[a-zA-Z0-9_-]+$/.test(form.id) && form.id.length > 0;
  const urlValid = form.url.startsWith("http://") || form.url.startsWith("https://");
  const canSubmit = idValid && urlValid;

  // 检查 ID 是否重复
  const idDuplicate = !isEdit && settings.sets.some((s) => s.id === form.id);

  return (
    <Modal
      open
      onClose={onClose}
      eyebrow={isEdit ? "编辑规则集" : "新增规则集"}
      title={isEdit ? form.name || form.id : "新增远程规则集"}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button onClick={() => onSubmit(form)} disabled={!canSubmit || idDuplicate}>
            {isEdit ? "保存" : "添加"}
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Field
            label="ID"
            hint="唯一标识"
            error={idDuplicate ? "ID 已存在" : undefined}
          >
            <Input
              value={form.id}
              disabled={isEdit}
              onChange={(e) => update("id", e.target.value.trim())}
              placeholder="geosite-custom"
            />
          </Field>
          <Field label="显示名">
            <Input value={form.name} onChange={(e) => update("name", e.target.value)} placeholder="自定义规则" />
          </Field>
        </div>

        <Field label="远程 URL" hint="数据平面将直接拉取此 URL">
          <Input
            value={form.url}
            onChange={(e) => update("url", e.target.value)}
            placeholder="https://raw.githubusercontent.com/..."
          />
        </Field>

        <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Field label="分类">
            <Select value={form.kind} onChange={(e) => update("kind", e.target.value as RemoteRuleSet["kind"])}>
              <option value="geo">Geo 规则</option>
              <option value="adblock">广告拦截</option>
              <option value="custom">自定义</option>
              <option value="provider">订阅附带</option>
            </Select>
          </Field>
          <Field label="目标出口">
            <Select value={form.target} onChange={(e) => update("target", e.target.value as RemoteRuleTarget)}>
              <option value="direct">直连 (Direct)</option>
              <option value="proxy">代理 (Proxy)</option>
              <option value="block">拦截 (Block)</option>
            </Select>
          </Field>
          <Field label="格式">
            <Select value={form.format || "binary"} onChange={(e) => update("format", e.target.value as RemoteRuleFormat)}>
              <option value="binary">二进制 (.srs)</option>
              <option value="source">源文本 (.json)</option>
            </Select>
          </Field>
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Field label="下载出口">
            <Select
              value={form.download_detour || "Direct"}
              onChange={(e) => update("download_detour", e.target.value)}
            >
              <option value="Direct">直连</option>
              <option value="Proxy">代理</option>
            </Select>
          </Field>
          <Field label="更新间隔">
            <Input
              value={form.update_interval || ""}
              onChange={(e) => update("update_interval", e.target.value)}
              placeholder="24h"
            />
          </Field>
          <Field label="预估大小 (字节)">
            <Input
              type="number"
              value={form.estimated_bytes || 0}
              onChange={(e) => update("estimated_bytes", Number(e.target.value) || 0)}
              placeholder="819200"
            />
          </Field>
        </div>

        <Field label="启用">
          <div className="h-10 flex items-center">
            <Toggle on={form.enabled} onChange={(v) => update("enabled", v)} />
          </div>
        </Field>
      </div>
    </Modal>
  );
}

// ===== utils =====

function kindLabel(k: string) {
  switch (k) {
    case "geo": return "Geo";
    case "adblock": return "广告";
    case "custom": return "自定义";
    case "provider": return "订阅";
    default: return k;
  }
}

function kindTone(k: string): "ok" | "warn" | "err" | "info" | "muted" | "accent" {
  switch (k) {
    case "geo": return "info";
    case "adblock": return "warn";
    case "custom": return "accent";
    case "provider": return "muted";
    default: return "muted";
  }
}

function targetLabel(t: string) {
  switch (t) {
    case "direct": return "直连";
    case "proxy": return "代理";
    case "block": return "拦截";
    default: return t;
  }
}

function targetTone(t: string): "ok" | "warn" | "err" | "info" | "muted" | "accent" {
  switch (t) {
    case "direct": return "ok";
    case "proxy": return "accent";
    case "block": return "err";
    default: return "muted";
  }
}
