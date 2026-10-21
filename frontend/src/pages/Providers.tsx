import { useMemo, useState } from "react";
import {
  useProviders,
  useProvidersApply,
  useProviderRefresh,
  useProviderRefreshAll,
  useProviderLogs,
} from "../lib/queries";
import type {
  Provider,
  ProviderChange,
  ProviderFetchMode,
  ProviderInput,
  ProviderType,
  ProviderRefreshLog,
  ProviderRefreshResult,
  ProviderSubscriptionInfo,
} from "../lib/types";
import { Card, SectionHeader, Eyebrow, Rule } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Modal } from "../ui/Modal";
import { Field, Input, Select } from "../ui/Field";
import { Toggle } from "../ui/Segmented";
import { serif, sans, eyebrowStyle } from "../ui/tokens";
import { ErrorText } from "../ui/ErrorText";
import { SUBSCRIPTION_UA_PRESETS } from "../lib/coreLabels";

type EditState = { mode: "create" } | { mode: "edit"; provider: Provider } | null;

export function ProvidersPage() {
  const providers = useProviders();
  const logs = useProviderLogs();
  const refresh = useProviderRefresh();
  const refreshAll = useProviderRefreshAll();
  const apply = useProvidersApply();

  const [edit, setEdit] = useState<EditState>(null);
  const [confirmDelete, setConfirmDelete] = useState<Provider | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState("");

  async function submitChange(change: ProviderChange) {
    setError(null);
    try {
      await apply.mutateAsync({ changes: [change] });
      setEdit(null);
      setConfirmDelete(null);
    } catch (e) {
      setError(e);
    }
  }

  return (
    <div className="space-y-12 max-w-[1120px]">
      <SectionHeader
        eyebrow="订阅 · Providers"
        title="代理来源"
        desc="订阅、手动节点、本地文件，按优先级合并后产出节点池。"
        right={
          <div className="flex items-center gap-3">
            <Button
              size="sm"
              variant="ghost"
              loading={refreshAll.isPending}
              onClick={() => {
                setError(null);
                setNotice("");
                refreshAll.mutate(undefined, {
                  onSuccess: (result) => {
                    const summary = summarizeRefreshResult(result);
                    if (summary.level === "error") setError(summary.message);
                    else if (summary.level === "info") setNotice(summary.message);
                  },
                  onError: (e) => setError(e),
                });
              }}
            >
              刷新全部
            </Button>
            <Button size="sm" onClick={() => setEdit({ mode: "create" })}>
              新增
            </Button>
          </div>
        }
      />

      {error ? <ErrorRow error={error} onDismiss={() => setError(null)} /> : null}
      {notice ? <NoticeRow message={notice} onDismiss={() => setNotice("")} /> : null}

      <ProvidersTable
        items={providers.data?.providers ?? []}
        loading={providers.isLoading}
        refreshing={refresh.isPending ? refresh.variables : null}
        onEdit={(p) => setEdit({ mode: "edit", provider: p })}
        onDelete={(p) => setConfirmDelete(p)}
        onRefresh={(p) => {
          setError(null);
          setNotice("");
          refresh.mutate(p.id, {
            onSuccess: (result) => {
              const summary = summarizeRefreshResult(result);
              if (summary.level === "error") setError(summary.message);
              else if (summary.level === "info") setNotice(summary.message);
            },
            onError: (e) => setError(e),
          });
        }}
      />

      <Rule />

      <section className="space-y-6">
        <SectionHeader
          eyebrow="刷新日志"
          title="最近事件"
          desc="保留最近的订阅刷新记录，由后端 runtime state 提供。"
        />
        <RefreshLogList items={logs.data?.logs ?? []} loading={logs.isLoading} />
      </section>

      {edit ? (
        <ProviderEditModal
          initial={edit.mode === "edit" ? edit.provider : undefined}
          onClose={() => setEdit(null)}
          onSubmit={(p) => submitChange({ action: "upsert", provider: p })}
          loading={apply.isPending}
        />
      ) : null}

      {confirmDelete ? (
        <ConfirmDeleteModal
          provider={confirmDelete}
          loading={apply.isPending}
          onClose={() => setConfirmDelete(null)}
          onConfirm={() =>
            submitChange({ action: "delete", id: confirmDelete.id })
          }
        />
      ) : null}
    </div>
  );
}

function ProvidersTable({
  items,
  loading,
  refreshing,
  onEdit,
  onDelete,
  onRefresh,
}: {
  items: Provider[];
  loading: boolean;
  refreshing: string | null | undefined;
  onEdit: (p: Provider) => void;
  onDelete: (p: Provider) => void;
  onRefresh: (p: Provider) => void;
}) {
  if (loading && items.length === 0) {
    return <EmptyCard text="载入中…" />;
  }
  if (items.length === 0) {
    return <EmptyCard text={`还没有订阅，点右上角"新增"添加。`} />;
  }
  return (
    <Card>
      <div className="overflow-x-auto">
      <table className="w-full text-sm min-w-[720px]" style={sans}>
        <thead>
          <tr style={{ color: "var(--muted)", borderBottom: "1px solid var(--border)" }}>
            <th className="text-left px-6 py-3 font-normal text-xs uppercase tracking-widest">名称</th>
            <th className="text-left px-3 py-3 font-normal text-xs uppercase tracking-widest">类型</th>
            <th className="text-left px-3 py-3 font-normal text-xs uppercase tracking-widest">来源</th>
            <th className="text-left px-3 py-3 font-normal text-xs uppercase tracking-widest">优先级</th>
            <th className="text-left px-3 py-3 font-normal text-xs uppercase tracking-widest">启用</th>
            <th className="text-right px-6 py-3 font-normal text-xs uppercase tracking-widest">操作</th>
          </tr>
        </thead>
        <tbody>
          {items.map((p, i) => (
            <tr
              key={p.id}
              style={{
                borderBottom: i === items.length - 1 ? "none" : "1px solid var(--border)",
              }}
            >
              <td className="px-6 py-4">
                <p className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                  {p.name || p.id}
                </p>
                <p className="text-[11px] mt-0.5" style={{ color: "var(--muted)" }}>
                  {p.id}
                </p>
                {p.roles?.length ? (
                  <div className="mt-2 flex flex-wrap gap-1">
                    {p.roles.map((r) => (
                      <Badge key={r} tone="muted">{r}</Badge>
                    ))}
                  </div>
                ) : null}
                {p.subscription ? (
                  <SubscriptionSummary info={p.subscription} />
                ) : null}
              </td>
              <td className="px-3 py-4">
                <ProviderTypeBadge type={p.type} fetchMode={p.fetch_mode} />
              </td>
              <td className="px-3 py-4 max-w-[280px]">
                <p className="truncate text-xs" style={{ fontFamily: "var(--font-mono)", color: "var(--fg)" }}>
                  {providerSourceSummary(p)}
                </p>
              </td>
              <td className="px-3 py-4 tabular-nums" style={{ color: "var(--fg-strong)" }}>
                {p.priority}
              </td>
              <td className="px-3 py-4">
                {p.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="muted">停用</Badge>}
              </td>
              <td className="px-6 py-4">
                <div className="flex items-center justify-end gap-1.5 flex-nowrap">
                  {p.type === "subscription" || p.type === "local-file" ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      loading={refreshing === p.id}
                      onClick={() => onRefresh(p)}
                    >
                      刷新
                    </Button>
                  ) : null}
                  <Button size="sm" variant="ghost" onClick={() => onEdit(p)}>
                    编辑
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => onDelete(p)}>
                    删除
                  </Button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      </div>
    </Card>
  );
}

function ProviderTypeBadge({ type, fetchMode }: { type: ProviderType; fetchMode: ProviderFetchMode }) {
  const labelMap: Record<ProviderType, string> = {
    subscription: "订阅",
    manual: "手动",
    "local-file": "本地文件",
    temporary: "临时",
  };
  return (
    <div className="flex flex-col gap-1">
      <Badge tone="accent">{labelMap[type] || type}</Badge>
      {type === "subscription" ? (
        <span className="text-[10px] italic" style={{ ...serif, color: "var(--muted)" }}>
          {fetchMode === "via_bootstrap" ? "经引导" : "直连"}
        </span>
      ) : null}
    </div>
  );
}

function RefreshLogList({ items, loading }: { items: ProviderRefreshLog[]; loading: boolean }) {
  if (loading && items.length === 0) return <EmptyCard text="载入中…" />;
  if (items.length === 0) return <EmptyCard text="暂无刷新记录。" />;
  return (
    <Card>
      <ul>
        {items.slice(0, 30).map((it, i, arr) => (
          <li
            key={i}
            className="px-7 py-4 flex items-start gap-5"
            style={{
              borderBottom: i === arr.length - 1 ? "none" : "1px solid var(--border)",
            }}
          >
            <span className="w-32 text-xs tabular-nums pt-1" style={{ color: "var(--muted)" }}>
              {formatTime(it.time)}
            </span>
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2">
                <p className="text-[15px]" style={{ ...serif, color: "var(--fg-strong)" }}>
                  {it.provider_id || it.scope}
                </p>
                <RefreshStatusBadge status={it.status} changed={it.changed} />
              </div>
              {it.error ? (
                <p className="text-xs mt-1" style={{ color: "var(--danger)" }}>
                  刷新失败<span style={{ color: "var(--muted)" }}> · 诊断：{it.error}</span>
                </p>
              ) : it.count !== undefined ? (
                <p className="text-xs mt-1" style={{ color: "var(--muted)" }}>{it.count} 个节点</p>
              ) : null}
            </div>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function RefreshStatusBadge({ status, changed }: { status: string; changed: boolean }) {
  if (status === "ok") return <Badge tone="ok">{changed ? "已更新" : "正常"}</Badge>;
  if (status === "unchanged") return <Badge tone="muted">未变化</Badge>;
  if (status === "stale") return <Badge tone="warn">陈旧</Badge>;
  if (status === "failed") return <Badge tone="err">失败</Badge>;
  return <Badge tone="muted"><span title={status}>状态未知</span></Badge>;
}

function EmptyCard({ text }: { text: string }) {
  return (
    <Card>
      <div className="px-7 py-10 text-center">
        <p className="text-sm" style={{ ...sans, color: "var(--muted)" }}>{text}</p>
      </div>
    </Card>
  );
}

function NoticeRow({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return (
    <div
      className="px-5 py-3 flex items-start justify-between gap-4"
      style={{
        background: "color-mix(in srgb, var(--success) 10%, transparent)",
        border: "1px solid color-mix(in srgb, var(--success) 35%, transparent)",
      }}
    >
      <p className="text-sm" style={{ ...sans, color: "var(--fg)" }}>
        {message}
      </p>
      <button
        type="button"
        className="text-xs shrink-0"
        style={{ color: "var(--muted)" }}
        onClick={onDismiss}
      >
        关闭
      </button>
    </div>
  );
}

function ErrorRow({ error, onDismiss }: { error: unknown; onDismiss: () => void }) {
  return (
    <div
      className="px-5 py-3 flex items-start justify-between gap-4"
      style={{
        background: "color-mix(in srgb, var(--danger) 10%, transparent)",
        border: "1px solid color-mix(in srgb, var(--danger) 35%, transparent)",
      }}
    >
      <div>
        <p style={eyebrowStyle}>操作失败</p>
        <p className="break-words mt-1 text-sm" style={{ ...sans, color: "var(--fg)" }}>
          <ErrorText error={error} fallback="订阅操作失败" showDiagnostic />
        </p>
      </div>
      <Button size="sm" variant="ghost" onClick={onDismiss}>关闭</Button>
    </div>
  );
}

// ===== Edit modal =====

const UA_CUSTOM = "__custom__";
const UA_PRESETS = SUBSCRIPTION_UA_PRESETS;

function ProviderEditModal({
  initial,
  onClose,
  onSubmit,
  loading,
}: {
  initial?: Provider;
  onClose: () => void;
  onSubmit: (p: ProviderInput) => void;
  loading: boolean;
}) {
  const [form, setForm] = useState<ProviderForm>(() => providerForm(initial));
  const isEdit = Boolean(initial);
  const [touchedId, setTouchedId] = useState(false);
  const [uaPreset, setUaPreset] = useState<string>(() => {
    const ua = initial?.user_agent ?? "";
    if (!ua) return "";
    return UA_PRESETS.some((p) => p.value === ua) ? ua : UA_CUSTOM;
  });

  const idValid = /^[a-zA-Z0-9_-]+$/.test(form.id) && form.id.length > 0;
  const sourceLabel = useMemo(() => {
    if (form.type === "subscription") return "订阅 URL";
    if (form.type === "local-file") return "本地路径";
    return "";
  }, [form.type]);

  const showUrl = form.type === "subscription";
  const showFilePath = form.type === "local-file";
  const showFetchMode = form.type === "subscription";

  function update<K extends keyof ProviderForm>(k: K, v: ProviderForm[K]) {
    setForm((s) => ({ ...s, [k]: v }));
  }

  function submit() {
    if (!idValid) {
      setTouchedId(true);
      return;
    }
    onSubmit(buildProviderInput(form));
  }

  return (
    <Modal
      open
      onClose={onClose}
      eyebrow={isEdit ? "编辑订阅" : "新增订阅"}
      title={isEdit ? form.name || form.id : "新增订阅"}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button onClick={submit} loading={loading} disabled={!idValid}>
            {isEdit ? "保存并应用" : "添加并应用"}
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Field
            label="ID"
            hint="唯一标识，只允许字母/数字/_/-"
            error={touchedId && !idValid ? "ID 不能为空且只能含字母数字、下划线或短横" : undefined}
          >
            <Input
              value={form.id}
              disabled={isEdit}
              onChange={(e) => update("id", e.target.value.trim())}
              onBlur={() => setTouchedId(true)}
              placeholder="airport1"
            />
          </Field>
          <Field label="显示名">
            <Input value={form.name} onChange={(e) => update("name", e.target.value)} placeholder="Airport 1" />
          </Field>
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Field label="类型">
            <Select
              value={form.type}
              onChange={(e) => update("type", e.target.value as ProviderType)}
            >
              <option value="subscription">远程订阅</option>
              <option value="manual">手动添加</option>
              <option value="local-file">本地文件</option>
              <option value="temporary">临时节点</option>
            </Select>
          </Field>
          <Field label="优先级" hint="仅影响刷新顺序，数值越小越先刷">
            <Input
              type="number"
              value={form.priority}
              onChange={(e) => update("priority", Number(e.target.value) || 0)}
            />
          </Field>
          <Field label="启用">
            <div className="h-10 flex items-center">
              <Toggle on={form.enabled} onChange={(v) => update("enabled", v)} />
            </div>
          </Field>
        </div>

        {showUrl ? (
          <Field
            label={sourceLabel}
            hint={isEdit && initial?.url_configured ? `已配置${initial.url_masked ? `：${initial.url_masked}` : ""}；留空则保留原值` : undefined}
          >
            <Input
              value={form.url}
              onChange={(e) => update("url", e.target.value)}
              placeholder={isEdit && initial?.url_configured ? "输入新 URL 以替换" : "https://example.com/subscribe 或导入链接"}
            />
          </Field>
        ) : null}
        {showFilePath ? (
          <Field
            label={sourceLabel}
            hint={isEdit && initial?.file_path_configured ? "已配置；留空则保留原值" : undefined}
          >
            <Input
              value={form.file_path}
              onChange={(e) => update("file_path", e.target.value)}
              placeholder={isEdit && initial?.file_path_configured ? "输入新路径以替换" : "/etc/smartgate/manual.yaml"}
            />
          </Field>
        ) : null}
        {showFetchMode ? (
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <Field label="抓取模式">
              <Select
                value={form.fetch_mode}
                onChange={(e) => update("fetch_mode", e.target.value as ProviderFetchMode)}
              >
                <option value="direct">直连抓取</option>
                <option value="via_bootstrap">经引导节点代理抓取</option>
              </Select>
            </Field>
            <Field label="引导节点 ID" hint={"仅「经引导节点」模式生效"}>
              <Input
                value={form.bootstrap}
                onChange={(e) => update("bootstrap", e.target.value)}
                placeholder="airport0:bootstrap"
                disabled={form.fetch_mode !== "via_bootstrap"}
              />
            </Field>
          </div>
        ) : null}
        {showUrl ? (
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <Field label="请求身份（User-Agent）" hint="部分机场按客户端 UA 返回不同格式或拒绝请求">
              <Select
                value={uaPreset}
                onChange={(e) => {
                  const value = e.target.value;
                  setUaPreset(value);
                  update("user_agent", value === UA_CUSTOM ? form.user_agent ?? "" : value);
                }}
              >
                {UA_PRESETS.map((p) => (
                  <option key={p.value || "default"} value={p.value}>{p.label}</option>
                ))}
                <option value={UA_CUSTOM}>自定义…</option>
              </Select>
            </Field>
            {uaPreset === UA_CUSTOM ? (
              <Field label="自定义 UA" hint="仅限英文可见字符">
                <Input
                  value={form.user_agent ?? ""}
                  onChange={(e) => update("user_agent", e.target.value)}
                  placeholder={UA_PRESETS.find((p) => p.value)?.value ?? ""}
                />
              </Field>
            ) : null}
          </div>
        ) : null}

        <Field label="角色" hint="逗号分隔，决定该订阅参与哪些业务池">
          <Input
            value={form.roles.join(", ")}
            onChange={(e) => update("roles", splitCsv(e.target.value))}
            placeholder="general, ai"
          />
        </Field>

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Field label="包含关键词" hint="逗号分隔，仅保留含有这些词的节点">
            <Input
              value={form.include_keywords.join(", ")}
              onChange={(e) => update("include_keywords", splitCsv(e.target.value))}
              placeholder="hk, premium"
            />
          </Field>
          <Field label="排除关键词" hint="逗号分隔，过滤掉含有这些词的节点">
            <Input
              value={form.exclude_keywords.join(", ")}
              onChange={(e) => update("exclude_keywords", splitCsv(e.target.value))}
              placeholder="expire, low"
            />
          </Field>
        </div>
      </div>
    </Modal>
  );
}

function ConfirmDeleteModal({
  provider,
  loading,
  onClose,
  onConfirm,
}: {
  provider: Provider;
  loading: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Modal
      open
      onClose={onClose}
      eyebrow="删除订阅"
      title={`删除 ${provider.name || provider.id}？`}
      width={520}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button variant="danger" onClick={onConfirm} loading={loading}>
            确认删除
          </Button>
        </>
      }
    >
      <p className="text-sm leading-7" style={{ ...serif, color: "var(--fg)" }}>
        删除会立刻通过 providers/apply 进入提交链。删除后引用了这条订阅的节点将一并消失。
      </p>
      <p className="text-xs mt-3" style={{ ...sans, color: "var(--muted)" }}>
        ID <code style={{ fontFamily: "var(--font-mono)" }}>{provider.id}</code>
      </p>
    </Modal>
  );
}

// ===== utils =====

type ProviderForm = ProviderInput & {
  url: string;
  file_path: string;
};

function providerForm(initial?: Provider): ProviderForm {
  if (initial) {
    return {
      id: initial.id,
      name: initial.name,
      type: initial.type,
      enabled: initial.enabled,
      fetch_mode: initial.fetch_mode,
      roles: [...initial.roles],
      bootstrap: initial.bootstrap,
      url: "",
      file_path: "",
      user_agent: initial.user_agent ?? "",
      priority: initial.priority,
      include_keywords: [...initial.include_keywords],
      exclude_keywords: [...initial.exclude_keywords],
    };
  }
  return {
    id: "",
    name: "",
    type: "subscription",
    enabled: true,
    fetch_mode: "direct",
    roles: [],
    bootstrap: "",
    url: "",
    file_path: "",
    user_agent: "",
    priority: 10,
    include_keywords: [],
    exclude_keywords: [],
  };
}

function providerSourceSummary(provider: Provider): string {
  if (provider.type === "subscription") {
    if (provider.url_masked) return provider.url_masked;
    return provider.url_configured ? "已配置订阅 URL" : "未配置订阅 URL";
  }
  if (provider.type === "local-file") {
    return provider.file_path_configured ? "已配置本地路径" : "未配置本地路径";
  }
  return "无需来源";
}

function buildProviderInput(form: ProviderForm): ProviderInput {
  const provider: ProviderInput = {
    id: form.id,
    name: form.name,
    type: form.type,
    enabled: form.enabled,
    fetch_mode: form.fetch_mode,
    roles: dedupe(form.roles),
    bootstrap: form.bootstrap,
    priority: form.priority,
    include_keywords: dedupe(form.include_keywords),
    exclude_keywords: dedupe(form.exclude_keywords),
  };
  const url = form.url.trim();
  const filePath = form.file_path.trim();
  if (form.type === "subscription" && url) provider.url = url;
  if (form.type === "subscription") provider.user_agent = (form.user_agent ?? "").trim();
  if (form.type === "local-file" && filePath) provider.file_path = filePath;
  return provider;
}

function splitCsv(s: string): string[] {
  return s
    .split(/[,\s]+/g)
    .map((x) => x.trim())
    .filter(Boolean);
}

function dedupe(arr: string[]): string[] {
  return Array.from(new Set(arr));
}

function formatTime(s: string | undefined | null): string {
  if (!s) return "—";
  const d = new Date(s);
  return Number.isFinite(d.getTime()) ? d.toLocaleString() : "—";
}


type RefreshSummary = { level: "none" | "info" | "error"; message: string };

function summarizeRefreshResult(result: ProviderRefreshResult): RefreshSummary {
  const rows = result.results || [];
  const bad = rows.filter((r) => r.status === "stale" || r.status === "failed");
  const metadataOnly = rows.filter((r) => r.metadata_only).map((r) => r.provider_id);
  const subErrs = rows
    .filter((r) => r.subscription_error)
    .map((r) => `${r.provider_id}（诊断：${r.subscription_error}）`);
  const parts: string[] = [];
  if (bad.length) {
    parts.push(
      bad
        .map((r) => `${r.provider_id} ${r.status === "stale" ? "陈旧" : "失败"}${r.error ? `（诊断：${r.error}）` : ""}`)
        .join("；"),
    );
  }
  if (subErrs.length) {
    parts.push(`订阅信息解析失败：${subErrs.join("；")}`);
  }
  if (parts.length) {
    if (metadataOnly.length) {
      parts.push(`仅名称/来源变化：${metadataOnly.join("、")}（metadata-only，不影响运行）。`);
    }
    if (result.changed) {
      parts.push("其余 provider 已更新到 last-good；若顶部显示待应用，请演练并应用后才会进入数据面。");
    }
    return { level: "error", message: parts.join(" ") };
  }
  if (metadataOnly.length) {
    const changedNodes = rows.some((row) => row.changed && !row.metadata_only);
    return {
      level: "info",
      message: changedNodes
        ? `仅名称/来源变化：${metadataOnly.join("、")}（metadata-only，不影响运行）；其余节点更新已写入 last-good。`
        : `仅名称/来源变化：${metadataOnly.join("、")}（metadata-only，不影响运行）。`,
    };
  }
  if (result.changed) {
    return {
      level: "info",
      message: "节点已更新到 last-good；若顶部显示待应用，请演练并应用后才会进入数据面。",
    };
  }
  return { level: "none", message: "" };
}

function SubscriptionSummary({ info }: { info: ProviderSubscriptionInfo }) {
  const total = info.total_bytes || 0;
  const used = info.used_bytes || 0;
  const remaining = info.remaining_bytes;
  const hasQuota = total > 0;
  const pct = hasQuota ? Math.min((used / total) * 100, 100) : 0;
  const tone: "ok" | "warn" | "err" =
    pct >= 90 ? "err" : pct >= 70 ? "warn" : "ok";
  const barColor =
    tone === "err" ? "var(--danger)" : tone === "warn" ? "var(--warning)" : "var(--success)";

  const expireStatus = expireInfo(info);

  // 配额、已用、到期、更新时间都没有 → 不渲染，避免一个近乎空白的块
  if (!hasQuota && used <= 0 && !expireStatus && !info.updated_at) return null;

  return (
    <div className="mt-3 space-y-1.5 max-w-[260px] whitespace-nowrap">
      {hasQuota ? (
        <>
          <div className="flex items-baseline justify-between gap-2 text-[11px] tabular-nums" style={sans}>
            <span style={{ color: "var(--muted)" }}>剩余流量</span>
            <span style={{ color: "var(--fg-strong)" }}>
              <span style={{ color: barColor }}>{formatBytes(Math.max(remaining ?? total - used, 0))}</span>
              <span style={{ color: "var(--muted)" }}> / {formatBytes(total)}</span>
            </span>
          </div>
          <div className="h-1 rounded-full overflow-hidden" style={{ background: "var(--surface-2)" }}>
            <div className="h-full rounded-full" style={{ width: `${pct}%`, background: barColor }} />
          </div>
        </>
      ) : used > 0 ? (
        <div className="flex items-baseline justify-between gap-2 text-[11px] tabular-nums" style={sans}>
          <span style={{ color: "var(--muted)" }}>已用流量</span>
          <span style={{ color: "var(--fg-strong)" }}>{formatBytes(used)}</span>
        </div>
      ) : null}
      {expireStatus ? (
        <div className="flex items-center gap-1.5 text-[11px]" style={sans}>
          <span style={{ color: "var(--muted)" }}>到期</span>
          <span style={{ color: expireStatus.tone === "err" ? "var(--danger)" : expireStatus.tone === "warn" ? "var(--warning)" : "var(--fg)" }}>
            {expireStatus.text}
          </span>
        </div>
      ) : null}
      {info.updated_at ? (
        <div className="text-[10px]" style={{ ...sans, color: "var(--muted)" }}>
          更新于 {formatTime(info.updated_at)}
        </div>
      ) : null}
    </div>
  );
}

function expireInfo(info: ProviderSubscriptionInfo): { text: string; tone: "ok" | "warn" | "err" } | null {
  let ms: number | null = null;
  if (info.expire_unix && info.expire_unix > 0) {
    ms = info.expire_unix * 1000;
  } else if (info.expire_at) {
    const t = new Date(info.expire_at).getTime();
    if (Number.isFinite(t)) ms = t;
  }
  if (ms === null) return null;

  const date = new Date(ms).toLocaleDateString();
  const diffMs = ms - Date.now();
  const days = Math.floor(diffMs / 86400000);
  if (diffMs <= 0) return { text: `${date} · 已过期`, tone: "err" };
  if (days <= 3) return { text: `${date} · 剩 ${days} 天`, tone: "err" };
  if (days <= 7) return { text: `${date} · 剩 ${days} 天`, tone: "warn" };
  return { text: `${date} · 剩 ${days} 天`, tone: "ok" };
}

function formatBytes(b: number): string {
  if (!Number.isFinite(b) || b < 0) return "—";
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  if (b < 1024 * 1024 * 1024) return `${(b / (1024 * 1024)).toFixed(1)} MB`;
  if (b < 1024 * 1024 * 1024 * 1024) return `${(b / (1024 * 1024 * 1024)).toFixed(2)} GB`;
  return `${(b / (1024 * 1024 * 1024 * 1024)).toFixed(2)} TB`;
}

// 占位，避免 Eyebrow 未使用
void Eyebrow;
